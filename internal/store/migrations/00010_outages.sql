-- +goose Up
-- Outages: each period in failover (from the failover until the failback).
-- ended is NULL while it lasts; at most one is open. started_by and ended_by
-- name the user of "Fail over now" or "Restore now", '' when automatic.
-- isp_id is the first non-primary ISP seen during it (no foreign key: ISPs
-- may be regrouped, and the report shows what is left).
CREATE TABLE outages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    started    INTEGER NOT NULL,
    ended      INTEGER,
    isp_id     INTEGER,
    started_by TEXT    NOT NULL DEFAULT '',
    ended_by   TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX outages_started ON outages (started);
CREATE UNIQUE INDEX outages_one_open ON outages ((ended IS NULL)) WHERE ended IS NULL;

-- Network bytes each container received (rx) and sent (tx) during an outage,
-- by container key. last_* are Docker's counters at the last sample, for the
-- next difference; ts is the last sample.
CREATE TABLE outage_usage (
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

-- +goose Down
DROP TABLE outage_usage;
DROP TABLE outages;
