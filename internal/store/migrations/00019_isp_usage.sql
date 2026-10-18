-- +goose Up
-- Network bytes each container received (rx) and sent (tx) while on a
-- metered primary ISP with a monthly data limit, by calendar month
-- ("YYYY-MM", server time). last_* are Docker's counters at the last sample,
-- for the next difference; ts is the last sample.
CREATE TABLE isp_usage (
    isp_id       INTEGER NOT NULL,
    month        TEXT    NOT NULL,
    host_id      INTEGER NOT NULL DEFAULT 1,
    container    TEXT    NOT NULL,
    container_id TEXT    NOT NULL,
    rx           INTEGER NOT NULL DEFAULT 0,
    tx           INTEGER NOT NULL DEFAULT 0,
    last_rx      INTEGER NOT NULL,
    last_tx      INTEGER NOT NULL,
    ts           INTEGER NOT NULL,
    PRIMARY KEY (isp_id, month, host_id, container)
);

-- +goose Down
DROP TABLE isp_usage;
