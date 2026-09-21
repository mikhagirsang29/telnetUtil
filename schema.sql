-- Hosts are the machines running telnetAgent. The IP is the natural key.
CREATE TABLE IF NOT EXISTS hosts (
    ip          TEXT PRIMARY KEY,
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Named groups of hosts
CREATE TABLE IF NOT EXISTS host_groups (
    name        TEXT PRIMARY KEY,
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Many-to-many membership. Deleting a host or a group cascades here,
-- so removing an IP automatically removes it from every group it was in.
CREATE TABLE IF NOT EXISTS host_group_members (
    group_name TEXT NOT NULL REFERENCES host_groups (name) ON DELETE CASCADE ON UPDATE CASCADE,
    host_ip    TEXT NOT NULL REFERENCES hosts (ip)         ON DELETE CASCADE ON UPDATE CASCADE,
    PRIMARY KEY (group_name, host_ip)
);

CREATE INDEX IF NOT EXISTS host_group_members_host_ip_idx ON host_group_members (host_ip);
