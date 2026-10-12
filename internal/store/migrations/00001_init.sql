-- +goose Up
-- Timestamps are Unix seconds (UTC); days are 'YYYY-MM-DD' in UTC.

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- An ISP is a group of ASNs: big providers announce from several (Examplenet:
-- AS64500, AS64501, ...). Roles, pinning and learning apply to the group.
CREATE TABLE isps (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    group_key    TEXT    NOT NULL UNIQUE,
    name         TEXT    NOT NULL,
    display_name TEXT    NOT NULL DEFAULT '',
    role         TEXT    NOT NULL DEFAULT 'backup' CHECK (role IN ('primary', 'backup', 'ignored')),
    pinned       INTEGER NOT NULL DEFAULT 0,
    first_seen   INTEGER NOT NULL,
    last_seen    INTEGER NOT NULL
);
CREATE UNIQUE INDEX isps_one_primary ON isps (role) WHERE role = 'primary';

CREATE TABLE asns (
    asn              INTEGER PRIMARY KEY,
    name             TEXT    NOT NULL DEFAULT '',
    isp_id           INTEGER NOT NULL REFERENCES isps (id),
    manual           INTEGER NOT NULL DEFAULT 0, -- grouped by hand; never regrouped
    observed_seconds INTEGER NOT NULL DEFAULT 0,
    first_seen       INTEGER NOT NULL,
    last_seen        INTEGER NOT NULL
);
CREATE INDEX asns_isp ON asns (isp_id);

CREATE TABLE asn_daily (
    asn              INTEGER NOT NULL,
    day              TEXT    NOT NULL,
    observed_seconds INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (asn, day)
);

CREATE TABLE checks (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    ts         INTEGER NOT NULL,
    ip         TEXT,
    asn        INTEGER,
    conclusive INTEGER NOT NULL,
    providers  TEXT    NOT NULL,
    latency_ms INTEGER NOT NULL
);
CREATE INDEX checks_ts ON checks (ts);

CREATE TABLE wan_state (
    id                  INTEGER PRIMARY KEY CHECK (id = 1),
    state               TEXT    NOT NULL,
    since               INTEGER NOT NULL,
    current_asn         INTEGER,
    pending_count       INTEGER NOT NULL DEFAULT 0,
    learning_started_at INTEGER NOT NULL
);

CREATE TABLE events (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    ts      INTEGER NOT NULL,
    type    TEXT    NOT NULL,
    actor   TEXT    NOT NULL DEFAULT 'system',
    payload TEXT    NOT NULL DEFAULT '{}'
);
CREATE INDEX events_ts ON events (ts);

-- +goose Down
DROP TABLE events;
DROP TABLE wan_state;
DROP TABLE checks;
DROP TABLE asn_daily;
DROP TABLE asns;
DROP TABLE isps;
DROP TABLE settings;
