-- +goose Up

CREATE TABLE stacks (
    project             TEXT PRIMARY KEY,
    disable_on_failover INTEGER NOT NULL DEFAULT 0,
    updated_at          INTEGER NOT NULL,
    updated_by          TEXT    NOT NULL DEFAULT 'system'
);

-- key = project/service for Compose containers, the container name otherwise.
CREATE TABLE containers (
    key          TEXT PRIMARY KEY,
    project      TEXT    NOT NULL DEFAULT '',
    service      TEXT    NOT NULL DEFAULT '',
    name         TEXT    NOT NULL,
    policy       TEXT    NOT NULL DEFAULT 'inherit' CHECK (policy IN ('inherit', 'disable', 'keep')),
    needs_review INTEGER NOT NULL DEFAULT 0,
    first_seen   INTEGER NOT NULL,
    last_seen    INTEGER NOT NULL
);
CREATE INDEX containers_project ON containers (project);

CREATE TABLE runs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at INTEGER NOT NULL,
    ended_at   INTEGER,
    trigger    TEXT    NOT NULL,
    to_asn     INTEGER,
    status     TEXT    NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'completed', 'failed'))
);
CREATE UNIQUE INDEX runs_one_open ON runs (status) WHERE status = 'open';

CREATE TABLE run_actions (
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

-- +goose Down
DROP TABLE run_actions;
DROP TABLE runs;
DROP TABLE containers;
DROP TABLE stacks;
