-- +goose Up
-- A host is one place Drawbridge controls: host 1 is the local Docker socket.
-- status is updated each time the host is listed; last_error says why it
-- could not be.
CREATE TABLE hosts (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL UNIQUE,
    kind       TEXT    NOT NULL CHECK (kind IN ('local', 'agent')),
    platform   TEXT    NOT NULL DEFAULT '',
    status     TEXT    NOT NULL DEFAULT 'pending'
               CHECK (status IN ('pending', 'online', 'offline', 'revoked', 'disabled')),
    last_seen  INTEGER,
    last_error TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    created_by TEXT    NOT NULL DEFAULT 'system'
);
INSERT INTO hosts (id, name, kind, platform, status, created_at) VALUES (1, 'local', 'local', 'docker', 'online', unixepoch());

-- Containers and stacks belong to a host; existing ones to the local host.
-- SQLite cannot change a primary key, so the tables are rebuilt.
CREATE TABLE containers_new (
    host_id      INTEGER NOT NULL DEFAULT 1,
    key          TEXT    NOT NULL,
    project      TEXT    NOT NULL DEFAULT '',
    service      TEXT    NOT NULL DEFAULT '',
    name         TEXT    NOT NULL,
    policy       TEXT    NOT NULL DEFAULT 'inherit' CHECK (policy IN ('inherit', 'disable', 'keep')),
    needs_review INTEGER NOT NULL DEFAULT 0,
    first_seen   INTEGER NOT NULL,
    last_seen    INTEGER NOT NULL,
    label_policy TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (host_id, key)
);
INSERT INTO containers_new (host_id, key, project, service, name, policy, needs_review, first_seen, last_seen, label_policy)
    SELECT 1, key, project, service, name, policy, needs_review, first_seen, last_seen, label_policy FROM containers;
DROP TABLE containers;
ALTER TABLE containers_new RENAME TO containers;
CREATE INDEX containers_project ON containers (host_id, project);

CREATE TABLE stacks_new (
    host_id             INTEGER NOT NULL DEFAULT 1,
    project             TEXT    NOT NULL,
    disable_on_failover INTEGER NOT NULL DEFAULT 0,
    updated_at          INTEGER NOT NULL,
    updated_by          TEXT    NOT NULL DEFAULT 'system',
    PRIMARY KEY (host_id, project)
);
INSERT INTO stacks_new (host_id, project, disable_on_failover, updated_at, updated_by)
    SELECT 1, project, disable_on_failover, updated_at, updated_by FROM stacks;
DROP TABLE stacks;
ALTER TABLE stacks_new RENAME TO stacks;

-- restore is what starting the workload again needs (empty for Docker
-- containers). pending marks a start owed to a host that could not be
-- reached when the run ended; it is done when the host is back.
CREATE TABLE run_actions_new (
    run_id        INTEGER NOT NULL REFERENCES runs (id),
    host_id       INTEGER NOT NULL DEFAULT 1,
    container_id  TEXT    NOT NULL,
    container_key TEXT    NOT NULL,
    name          TEXT    NOT NULL,
    was_running   INTEGER NOT NULL,
    stopped_at    INTEGER,
    started_at    INTEGER,
    error         TEXT    NOT NULL DEFAULT '',
    restore       TEXT    NOT NULL DEFAULT '',
    pending       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (run_id, host_id, container_id)
);
INSERT INTO run_actions_new (run_id, host_id, container_id, container_key, name, was_running, stopped_at, started_at, error)
    SELECT run_id, 1, container_id, container_key, name, was_running, stopped_at, started_at, error FROM run_actions;
DROP TABLE run_actions;
ALTER TABLE run_actions_new RENAME TO run_actions;
CREATE INDEX run_actions_pending ON run_actions (pending) WHERE pending = 1;

CREATE TABLE outage_usage_new (
    outage_id    INTEGER NOT NULL,
    host_id      INTEGER NOT NULL DEFAULT 1,
    container    TEXT    NOT NULL,
    container_id TEXT    NOT NULL,
    rx           INTEGER NOT NULL DEFAULT 0,
    tx           INTEGER NOT NULL DEFAULT 0,
    last_rx      INTEGER NOT NULL,
    last_tx      INTEGER NOT NULL,
    ts           INTEGER NOT NULL,
    PRIMARY KEY (outage_id, host_id, container)
);
INSERT INTO outage_usage_new (outage_id, host_id, container, container_id, rx, tx, last_rx, last_tx, ts)
    SELECT outage_id, 1, container, container_id, rx, tx, last_rx, last_tx, ts FROM outage_usage;
DROP TABLE outage_usage;
ALTER TABLE outage_usage_new RENAME TO outage_usage;

-- +goose Down
CREATE TABLE outage_usage_old (
    outage_id    INTEGER NOT NULL,
    container    TEXT    NOT NULL,
    container_id TEXT    NOT NULL,
    rx           INTEGER NOT NULL DEFAULT 0,
    tx           INTEGER NOT NULL DEFAULT 0,
    last_rx      INTEGER NOT NULL,
    last_tx      INTEGER NOT NULL,
    ts           INTEGER NOT NULL,
    PRIMARY KEY (outage_id, container)
);
INSERT INTO outage_usage_old (outage_id, container, container_id, rx, tx, last_rx, last_tx, ts)
    SELECT outage_id, container, container_id, rx, tx, last_rx, last_tx, ts FROM outage_usage WHERE host_id = 1;
DROP TABLE outage_usage;
ALTER TABLE outage_usage_old RENAME TO outage_usage;

CREATE TABLE run_actions_old (
    run_id        INTEGER NOT NULL REFERENCES runs (id),
    container_id  TEXT    NOT NULL,
    container_key TEXT    NOT NULL,
    name          TEXT    NOT NULL,
    was_running   INTEGER NOT NULL,
    stopped_at    INTEGER,
    started_at    INTEGER,
    error         TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (run_id, container_id)
);
INSERT INTO run_actions_old (run_id, container_id, container_key, name, was_running, stopped_at, started_at, error)
    SELECT run_id, container_id, container_key, name, was_running, stopped_at, started_at, error FROM run_actions WHERE host_id = 1;
DROP TABLE run_actions;
ALTER TABLE run_actions_old RENAME TO run_actions;

CREATE TABLE stacks_old (
    project             TEXT PRIMARY KEY,
    disable_on_failover INTEGER NOT NULL DEFAULT 0,
    updated_at          INTEGER NOT NULL,
    updated_by          TEXT    NOT NULL DEFAULT 'system'
);
INSERT INTO stacks_old (project, disable_on_failover, updated_at, updated_by)
    SELECT project, disable_on_failover, updated_at, updated_by FROM stacks WHERE host_id = 1;
DROP TABLE stacks;
ALTER TABLE stacks_old RENAME TO stacks;

CREATE TABLE containers_old (
    key          TEXT PRIMARY KEY,
    project      TEXT    NOT NULL DEFAULT '',
    service      TEXT    NOT NULL DEFAULT '',
    name         TEXT    NOT NULL,
    policy       TEXT    NOT NULL DEFAULT 'inherit' CHECK (policy IN ('inherit', 'disable', 'keep')),
    needs_review INTEGER NOT NULL DEFAULT 0,
    first_seen   INTEGER NOT NULL,
    last_seen    INTEGER NOT NULL,
    label_policy TEXT    NOT NULL DEFAULT ''
);
INSERT INTO containers_old (key, project, service, name, policy, needs_review, first_seen, last_seen, label_policy)
    SELECT key, project, service, name, policy, needs_review, first_seen, last_seen, label_policy FROM containers WHERE host_id = 1;
DROP TABLE containers;
ALTER TABLE containers_old RENAME TO containers;
CREATE INDEX containers_project ON containers (project);

DROP TABLE hosts;
