-- +goose Up
-- The state machine became time-based: pending changes are timed, and
-- flapping is remembered so the failback wait can back off.
ALTER TABLE wan_state ADD COLUMN pending_since INTEGER NOT NULL DEFAULT 0;
ALTER TABLE wan_state ADD COLUMN last_evidence INTEGER NOT NULL DEFAULT 0;
ALTER TABLE wan_state ADD COLUMN flaps INTEGER NOT NULL DEFAULT 0;
ALTER TABLE wan_state ADD COLUMN last_failover INTEGER NOT NULL DEFAULT 0;
ALTER TABLE wan_state ADD COLUMN last_failback INTEGER NOT NULL DEFAULT 0;
ALTER TABLE wan_state DROP COLUMN pending_count;

-- +goose Down
ALTER TABLE wan_state ADD COLUMN pending_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE wan_state DROP COLUMN last_failback;
ALTER TABLE wan_state DROP COLUMN last_failover;
ALTER TABLE wan_state DROP COLUMN flaps;
ALTER TABLE wan_state DROP COLUMN last_evidence;
ALTER TABLE wan_state DROP COLUMN pending_since;
