package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"
)

const (
	maxPortsPerRequest = 50
	maxJobsPerRequest  = 2000 // agents x ports for one /telnet call
)

var (
	groupNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	destRe      = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,253}$`)
)

type Server struct {
	cfg   *Config
	store *Store
	agent *AgentClient
}

func NewServer(cfg *Config, store *Store, agent *AgentClient) *Server {
	return &Server{cfg: cfg, store: store, agent: agent}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.health)

	// hosts
	mux.HandleFunc("POST /hosts", s.createHosts)
	mux.HandleFunc("GET /hosts", s.listHosts)
	mux.HandleFunc("GET /hosts/{ip}", s.getHost)
	mux.HandleFunc("PATCH /hosts/{ip}", s.updateHost)
	mux.HandleFunc("DELETE /hosts/{ip}", s.deleteHost)
	mux.HandleFunc("POST /hosts/{ip}/groups", s.addHostToGroups)
	mux.HandleFunc("DELETE /hosts/{ip}/groups/{group}", s.removeMember)

	// groups
	mux.HandleFunc("POST /groups", s.createGroup)
	mux.HandleFunc("GET /groups", s.listGroups)
	mux.HandleFunc("GET /groups/{group}", s.getGroup)
	mux.HandleFunc("PATCH /groups/{group}", s.updateGroup)
	mux.HandleFunc("DELETE /groups/{group}", s.deleteGroup)
	mux.HandleFunc("POST /groups/{group}/hosts", s.addHostsToGroup)
	mux.HandleFunc("DELETE /groups/{group}/hosts/{ip}", s.removeMember)

	// run a telnet check from every agent in the given group(s)
	mux.HandleFunc("POST /telnet/{groups}", s.telnet)

	return s.logging(s.auth(mux))
}

// ---------------------------------------------------------------------------
// middleware
// ---------------------------------------------------------------------------

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
	})
}

// auth enforces the optional API key. /healthz is always open.
func (s *Server) auth(next http.Handler) http.Handler {
	if s.cfg.APIKey == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		key := r.Header.Get("X-API-Key")
		if key == "" {
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
				key = strings.TrimPrefix(h, "Bearer ")
			}
		}
		if subtle.ConstantTimeCompare([]byte(key), []byte(s.cfg.APIKey)) != 1 {
			writeError(w, http.StatusUnauthorized, "missing or invalid API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// response / request helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writing response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeStoreError(w http.ResponseWriter, err error) {
	var missing *MissingError
	switch {
	case errors.As(err, &missing):
		writeError(w, http.StatusNotFound, missing.Error())
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, ErrConflict):
		writeError(w, http.StatusConflict, "already exists")
	case isPgCode(err, "23503"):
		writeError(w, http.StatusConflict, "a referenced host or group no longer exists")
	default:
		log.Printf("internal error: %v", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}

// decodeBody parses a JSON body strictly (unknown fields are rejected so
// typos like "port" instead of "ports" don't pass silently).
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func normalizeIP(s string) (string, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil || addr.Zone() != "" {
		return "", fmt.Errorf("invalid IP address %q", s)
	}
	return addr.Unmap().String(), nil
}

func normalizeIPs(in []string) ([]string, error) {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		ip, err := normalizeIP(s)
		if err != nil {
			return nil, err
		}
		if !seen[ip] {
			seen[ip] = true
			out = append(out, ip)
		}
	}
	return out, nil
}

func normalizeGroupName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !groupNameRe.MatchString(s) {
		return "", fmt.Errorf("invalid group name %q (letters, digits, '.', '_' and '-' only, max 63 chars)", s)
	}
	return s, nil
}

func normalizeGroupNames(in []string) ([]string, error) {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		name, err := normalizeGroupName(s)
		if err != nil {
			return nil, err
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, nil
}

func normalizePorts(in []int) ([]int, error) {
	if len(in) == 0 {
		return nil, errors.New(`"ports" must contain at least one port`)
	}
	if len(in) > maxPortsPerRequest {
		return nil, fmt.Errorf(`"ports" may contain at most %d ports`, maxPortsPerRequest)
	}
	seen := make(map[int]bool, len(in))
	out := make([]int, 0, len(in))
	for _, p := range in {
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("invalid port %d (must be 1-65535)", p)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, nil
}

func pathIP(w http.ResponseWriter, r *http.Request) (string, bool) {
	ip, err := normalizeIP(r.PathValue("ip"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return "", false
	}
	return ip, true
}

func pathGroup(w http.ResponseWriter, r *http.Request) (string, bool) {
	name, err := normalizeGroupName(r.PathValue("group"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return "", false
	}
	return name, true
}

// ---------------------------------------------------------------------------
// health
// ---------------------------------------------------------------------------

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unreachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---------------------------------------------------------------------------
// hosts
// ---------------------------------------------------------------------------

// POST /hosts
//
//	{"ip": "10.0.0.1"}
//	{"ips": ["10.0.0.1", "10.0.0.2"], "groups": ["web", "prod"], "description": "web tier"}
//
// Idempotent: existing hosts are kept (description updated only if given) and
// simply added to the listed groups. Groups must already exist.
func (s *Server) createHosts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IP          string   `json:"ip"`
		IPs         []string `json:"ips"`
		Description *string  `json:"description"`
		Groups      []string `json:"groups"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	raw := append([]string{}, req.IPs...)
	if req.IP != "" {
		raw = append(raw, req.IP)
	}
	if len(raw) == 0 {
		writeError(w, http.StatusBadRequest, `provide "ip" or "ips"`)
		return
	}
	ips, err := normalizeIPs(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	groups, err := normalizeGroupNames(req.Groups)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	hosts, err := s.store.UpsertHosts(r.Context(), ips, req.Description, groups)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": hosts})
}

// GET /hosts            all hosts
// GET /hosts?group=web  only members of "web"
func (s *Server) listHosts(w http.ResponseWriter, r *http.Request) {
	group := strings.TrimSpace(r.URL.Query().Get("group"))
	if group != "" {
		var err error
		if group, err = normalizeGroupName(group); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	hosts, err := s.store.ListHosts(r.Context(), group)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": hosts})
}

func (s *Server) getHost(w http.ResponseWriter, r *http.Request) {
	ip, ok := pathIP(w, r)
	if !ok {
		return
	}
	host, err := s.store.GetHost(r.Context(), ip)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, host)
}

// PATCH /hosts/{ip}   {"description": "..."}
func (s *Server) updateHost(w http.ResponseWriter, r *http.Request) {
	ip, ok := pathIP(w, r)
	if !ok {
		return
	}
	var req struct {
		Description *string `json:"description"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Description == nil {
		writeError(w, http.StatusBadRequest, `provide "description"`)
		return
	}
	host, err := s.store.UpdateHost(r.Context(), ip, *req.Description)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, host)
}

// DELETE /hosts/{ip}  removes the host and (via cascade) all its group memberships.
func (s *Server) deleteHost(w http.ResponseWriter, r *http.Request) {
	ip, ok := pathIP(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteHost(r.Context(), ip); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /hosts/{ip}/groups   {"groups": ["web", "prod"]}   (one IP -> many groups)
func (s *Server) addHostToGroups(w http.ResponseWriter, r *http.Request) {
	ip, ok := pathIP(w, r)
	if !ok {
		return
	}
	var req struct {
		Groups []string `json:"groups"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	groups, err := normalizeGroupNames(req.Groups)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(groups) == 0 {
		writeError(w, http.StatusBadRequest, `"groups" must contain at least one group`)
		return
	}
	if err := s.store.AddMemberships(r.Context(), groups, []string{ip}); err != nil {
		writeStoreError(w, err)
		return
	}
	host, err := s.store.GetHost(r.Context(), ip)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, host)
}

// ---------------------------------------------------------------------------
// groups
// ---------------------------------------------------------------------------

// POST /groups   {"name": "web", "description": "...", "hosts": ["10.0.0.1"]}
func (s *Server) createGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Hosts       []string `json:"hosts"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	name, err := normalizeGroupName(req.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hosts, err := normalizeIPs(req.Hosts)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	group, err := s.store.CreateGroup(r.Context(), name, req.Description, hosts)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			writeError(w, http.StatusConflict, fmt.Sprintf("group %q already exists", name))
			return
		}
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, group)
}

func (s *Server) listGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.store.ListGroups(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

func (s *Server) getGroup(w http.ResponseWriter, r *http.Request) {
	name, ok := pathGroup(w, r)
	if !ok {
		return
	}
	group, err := s.store.GetGroup(r.Context(), name)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, group)
}

// PATCH /groups/{group}   {"name": "new-name", "description": "..."}  (both optional)
func (s *Server) updateGroup(w http.ResponseWriter, r *http.Request) {
	name, ok := pathGroup(w, r)
	if !ok {
		return
	}
	var req struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Name == nil && req.Description == nil {
		writeError(w, http.StatusBadRequest, `provide "name" and/or "description"`)
		return
	}
	var newName *string
	if req.Name != nil {
		n, err := normalizeGroupName(*req.Name)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		newName = &n
	}
	group, err := s.store.UpdateGroup(r.Context(), name, newName, req.Description)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			writeError(w, http.StatusConflict, "a group with that name already exists")
			return
		}
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, group)
}

// DELETE /groups/{group}  removes the group only, its hosts are kept.
func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request) {
	name, ok := pathGroup(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteGroup(r.Context(), name); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /groups/{group}/hosts   {"hosts": ["10.0.0.1", "10.0.0.2"]}   (many IPs -> one group)
func (s *Server) addHostsToGroup(w http.ResponseWriter, r *http.Request) {
	name, ok := pathGroup(w, r)
	if !ok {
		return
	}
	var req struct {
		Hosts []string `json:"hosts"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	hosts, err := normalizeIPs(req.Hosts)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(hosts) == 0 {
		writeError(w, http.StatusBadRequest, `"hosts" must contain at least one IP`)
		return
	}
	if err := s.store.AddMemberships(r.Context(), []string{name}, hosts); err != nil {
		writeStoreError(w, err)
		return
	}
	group, err := s.store.GetGroup(r.Context(), name)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, group)
}

// DELETE /groups/{group}/hosts/{ip}  and  DELETE /hosts/{ip}/groups/{group}
func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) {
	name, ok := pathGroup(w, r)
	if !ok {
		return
	}
	ip, ok := pathIP(w, r)
	if !ok {
		return
	}
	if err := s.store.RemoveMembership(r.Context(), name, ip); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("host %s is not a member of group %q", ip, name))
			return
		}
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// telnet fan-out
// ---------------------------------------------------------------------------

type telnetSummary struct {
	Total       int `json:"total"`
	Reachable   int `json:"reachable"`
	Unreachable int `json:"unreachable"`
}

type telnetResponse struct {
	Groups  []string       `json:"groups"`
	Dest    string         `json:"dest"`
	Ports   []int          `json:"ports"`
	Agents  int            `json:"agents"`
	Summary telnetSummary  `json:"summary"`
	Results []TelnetResult `json:"results"`
}

// POST /telnet/{groups}      groups = one name or a comma-separated list, e.g. /telnet/web,db
//
//	{"dest": "10.20.30.40", "ports": [22, 443]}
//
// Every agent in the union of those groups tests dest:port for every port.
func (s *Server) telnet(w http.ResponseWriter, r *http.Request) {
	groups, err := normalizeGroupNames(strings.Split(r.PathValue("groups"), ","))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var req struct {
		Dest  string `json:"dest"`
		Ports []int  `json:"ports"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	dest := strings.TrimSpace(req.Dest)
	if !destRe.MatchString(dest) {
		writeError(w, http.StatusBadRequest, `"dest" must be an IP address or hostname`)
		return
	}
	ports, err := normalizePorts(req.Ports)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	agents, err := s.store.HostsForGroups(r.Context(), groups)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if len(agents)*len(ports) > maxJobsPerRequest {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"%d agents x %d ports = %d checks, limit is %d per request",
			len(agents), len(ports), len(agents)*len(ports), maxJobsPerRequest))
		return
	}

	results := s.agent.CheckAll(r.Context(), agents, dest, ports, s.cfg.MaxConcurrency)

	summary := telnetSummary{Total: len(results)}
	for _, res := range results {
		if res.Reachable {
			summary.Reachable++
		} else {
			summary.Unreachable++
		}
	}

	writeJSON(w, http.StatusOK, telnetResponse{
		Groups:  groups,
		Dest:    dest,
		Ports:   ports,
		Agents:  len(agents),
		Summary: summary,
		Results: results,
	})
}
