-- +goose Up
-- A drawbridge.on-failover label on the container: '' (none), 'disable' or 'keep'.
ALTER TABLE containers ADD COLUMN label_policy TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE containers DROP COLUMN label_policy;
