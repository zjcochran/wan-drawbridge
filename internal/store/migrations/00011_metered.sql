-- +goose Up
-- Whether traffic on this ISP costs per byte. Only a metered ISP that is not
-- the primary triggers a failover; an unmetered one counts like the primary.
-- Every ISP starts metered, as before.
ALTER TABLE isps ADD COLUMN metered INTEGER NOT NULL DEFAULT 1;

-- +goose Down
ALTER TABLE isps DROP COLUMN metered;
