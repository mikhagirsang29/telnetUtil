package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("already exists")
)

// MissingError is returned when a request references hosts or groups that
// don't exist. It is reported to the client as a 404 listing the names.
type MissingError struct {
	Kind  string // "hosts" or "groups"
	Names []string
}

func (e *MissingError) Error() string {
	return fmt.Sprintf("unknown %s: %s", e.Kind, strings.Join(e.Names, ", "))
}

// querier is satisfied by both *pgxpool.Pool and pgx.Tx, so helpers can run
// inside or outside a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Migrate applies schema.sql. Every statement is idempotent (IF NOT EXISTS).
func (s *Store) Migrate(ctx context.Context) error {
	for _, stmt := range strings.Split(schemaSQL, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

func (s *Store) inTx(ctx context.Context, fn func(q querier) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // no-op after a successful Commit
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func isPgCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func findMissing(ctx context.Context, q querier, query string, wanted []string) ([]string, error) {
	if len(wanted) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, query, wanted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	found := make(map[string]bool, len(wanted))
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		found[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var missing []string
	for _, w := range wanted {
		if !found[w] {
			missing = append(missing, w)
		}
	}
	return missing, nil
}

func checkGroupsExist(ctx context.Context, q querier, groups []string) error {
	missing, err := findMissing(ctx, q, `SELECT name FROM host_groups WHERE name = ANY($1)`, groups)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return &MissingError{Kind: "groups", Names: missing}
	}
	return nil
}

func checkHostsExist(ctx context.Context, q querier, ips []string) error {
	missing, err := findMissing(ctx, q, `SELECT ip FROM hosts WHERE ip = ANY($1)`, ips)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return &MissingError{Kind: "hosts", Names: missing}
	}
	return nil
}

// addMemberships adds every host in hosts to every group in groups.
// Already-existing memberships are ignored, so it is safe to repeat.
func addMemberships(ctx context.Context, q querier, groups, hosts []string) error {
	if len(groups) == 0 || len(hosts) == 0 {
		return nil
	}
	if err := checkGroupsExist(ctx, q, groups); err != nil {
		return err
	}
	if err := checkHostsExist(ctx, q, hosts); err != nil {
		return err
	}
	_, err := q.Exec(ctx, `
		INSERT INTO host_group_members (group_name, host_ip)
		SELECT g.name, h.ip
		FROM unnest($1::text[]) AS g(name)
		CROSS JOIN unnest($2::text[]) AS h(ip)
		ON CONFLICT DO NOTHING`, groups, hosts)
	return err
}

const hostSelect = `
SELECT h.ip, h.description, h.created_at,
       COALESCE(array_agg(m.group_name ORDER BY m.group_name)
                FILTER (WHERE m.group_name IS NOT NULL), '{}'::text[])
FROM hosts h
LEFT JOIN host_group_members m ON m.host_ip = h.ip`

func queryHosts(ctx context.Context, q querier, where string, args ...any) ([]Host, error) {
	rows, err := q.Query(ctx, hostSelect+" "+where+" GROUP BY h.ip ORDER BY h.ip::inet", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	hosts := []Host{}
	for rows.Next() {
		var h Host
		if err := rows.Scan(&h.IP, &h.Description, &h.CreatedAt, &h.Groups); err != nil {
			return nil, err
		}
		if h.Groups == nil {
			h.Groups = []string{}
		}
		hosts = append(hosts, h)
	}
	return hosts, rows.Err()
}

const groupSelect = `
SELECT g.name, g.description, g.created_at,
       COALESCE(array_agg(m.host_ip ORDER BY m.host_ip::inet)
                FILTER (WHERE m.host_ip IS NOT NULL), '{}'::text[])
FROM host_groups g
LEFT JOIN host_group_members m ON m.group_name = g.name`

func queryGroups(ctx context.Context, q querier, where string, args ...any) ([]Group, error) {
	rows, err := q.Query(ctx, groupSelect+" "+where+" GROUP BY g.name ORDER BY g.name", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	groups := []Group{}
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.Name, &g.Description, &g.CreatedAt, &g.Hosts); err != nil {
			return nil, err
		}
		if g.Hosts == nil {
			g.Hosts = []string{}
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// ---------------------------------------------------------------------------
// hosts
// ---------------------------------------------------------------------------

// UpsertHosts creates the hosts (or updates their description if one is
// given), then adds them all to the listed groups. Groups must already exist.
func (s *Store) UpsertHosts(ctx context.Context, ips []string, description *string, groups []string) ([]Host, error) {
	var out []Host
	err := s.inTx(ctx, func(q querier) error {
		if err := checkGroupsExist(ctx, q, groups); err != nil {
			return err
		}
		for _, ip := range ips {
			if _, err := q.Exec(ctx, `
				INSERT INTO hosts (ip, description) VALUES ($1, COALESCE($2::text, ''))
				ON CONFLICT (ip) DO UPDATE SET description = COALESCE($2::text, hosts.description)`,
				ip, description); err != nil {
				return err
			}
		}
		if err := addMemberships(ctx, q, groups, ips); err != nil {
			return err
		}
		var qerr error
		out, qerr = queryHosts(ctx, q, "WHERE h.ip = ANY($1)", ips)
		return qerr
	})
	return out, err
}

// ListHosts returns all hosts, or only the members of one group.
func (s *Store) ListHosts(ctx context.Context, group string) ([]Host, error) {
	if group == "" {
		return queryHosts(ctx, s.pool, "")
	}
	if err := checkGroupsExist(ctx, s.pool, []string{group}); err != nil {
		return nil, err
	}
	return queryHosts(ctx, s.pool,
		"WHERE h.ip IN (SELECT host_ip FROM host_group_members WHERE group_name = $1)", group)
}

func (s *Store) GetHost(ctx context.Context, ip string) (*Host, error) {
	hosts, err := queryHosts(ctx, s.pool, "WHERE h.ip = $1", ip)
	if err != nil {
		return nil, err
	}
	if len(hosts) == 0 {
		return nil, ErrNotFound
	}
	return &hosts[0], nil
}

func (s *Store) UpdateHost(ctx context.Context, ip, description string) (*Host, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE hosts SET description = $2 WHERE ip = $1`, ip, description)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return s.GetHost(ctx, ip)
}

// DeleteHost removes the host. ON DELETE CASCADE also removes it from every
// group it belonged to.
func (s *Store) DeleteHost(ctx context.Context, ip string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM hosts WHERE ip = $1`, ip)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// groups
// ---------------------------------------------------------------------------

// CreateGroup creates a group, optionally with initial (existing) hosts.
func (s *Store) CreateGroup(ctx context.Context, name, description string, hosts []string) (*Group, error) {
	err := s.inTx(ctx, func(q querier) error {
		if _, err := q.Exec(ctx,
			`INSERT INTO host_groups (name, description) VALUES ($1, $2)`, name, description); err != nil {
			if isPgCode(err, "23505") {
				return ErrConflict
			}
			return err
		}
		return addMemberships(ctx, q, []string{name}, hosts)
	})
	if err != nil {
		return nil, err
	}
	return s.GetGroup(ctx, name)
}

func (s *Store) ListGroups(ctx context.Context) ([]Group, error) {
	return queryGroups(ctx, s.pool, "")
}

func (s *Store) GetGroup(ctx context.Context, name string) (*Group, error) {
	groups, err := queryGroups(ctx, s.pool, "WHERE g.name = $1", name)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, ErrNotFound
	}
	return &groups[0], nil
}

// UpdateGroup renames and/or changes the description. Membership follows a
// rename automatically (ON UPDATE CASCADE).
func (s *Store) UpdateGroup(ctx context.Context, name string, newName, description *string) (*Group, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE host_groups
		SET name = COALESCE($2::text, name),
		    description = COALESCE($3::text, description)
		WHERE name = $1`, name, newName, description)
	if err != nil {
		if isPgCode(err, "23505") {
			return nil, ErrConflict
		}
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	final := name
	if newName != nil {
		final = *newName
	}
	return s.GetGroup(ctx, final)
}

// DeleteGroup removes the group only. The hosts themselves are kept.
func (s *Store) DeleteGroup(ctx context.Context, name string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM host_groups WHERE name = $1`, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// membership
// ---------------------------------------------------------------------------

// AddMemberships adds every listed host to every listed group (idempotent).
func (s *Store) AddMemberships(ctx context.Context, groups, hosts []string) error {
	return s.inTx(ctx, func(q querier) error {
		return addMemberships(ctx, q, groups, hosts)
	})
}

func (s *Store) RemoveMembership(ctx context.Context, group, ip string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM host_group_members WHERE group_name = $1 AND host_ip = $2`, group, ip)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// HostsForGroups returns the de-duplicated union of hosts in the given groups.
// It fails with *MissingError if any group doesn't exist.
func (s *Store) HostsForGroups(ctx context.Context, groups []string) ([]string, error) {
	if err := checkGroupsExist(ctx, s.pool, groups); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT host_ip FROM host_group_members
		WHERE group_name = ANY($1) ORDER BY host_ip`, groups)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ips := []string{}
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			return nil, err
		}
		ips = append(ips, ip)
	}
	return ips, rows.Err()
}
