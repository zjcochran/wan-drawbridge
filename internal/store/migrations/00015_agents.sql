-- +goose Up
-- What an agent host needs: its version, its Ed25519 key (and the one it
-- replaces, until the new one is used), the one-time enrollment token (as a
-- hash), where its last request came from, the workloads it last pushed, and
-- which manager key it has pinned.
ALTER TABLE hosts ADD COLUMN version        TEXT NOT NULL DEFAULT '';
ALTER TABLE hosts ADD COLUMN public_key     TEXT NOT NULL DEFAULT '';
ALTER TABLE hosts ADD COLUMN previous_key   TEXT NOT NULL DEFAULT '';
ALTER TABLE hosts ADD COLUMN key_created    INTEGER;
ALTER TABLE hosts ADD COLUMN enroll_hash    TEXT NOT NULL DEFAULT '';
ALTER TABLE hosts ADD COLUMN enroll_expires INTEGER;
ALTER TABLE hosts ADD COLUMN last_address   TEXT NOT NULL DEFAULT '';
ALTER TABLE hosts ADD COLUMN inventory      TEXT NOT NULL DEFAULT '[]';
ALTER TABLE hosts ADD COLUMN manager_key_id TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX hosts_enroll_hash ON hosts (enroll_hash) WHERE enroll_hash <> '';

-- +goose Down
DROP INDEX hosts_enroll_hash;
ALTER TABLE hosts DROP COLUMN manager_key_id;
ALTER TABLE hosts DROP COLUMN inventory;
ALTER TABLE hosts DROP COLUMN last_address;
ALTER TABLE hosts DROP COLUMN enroll_expires;
ALTER TABLE hosts DROP COLUMN enroll_hash;
ALTER TABLE hosts DROP COLUMN key_created;
ALTER TABLE hosts DROP COLUMN previous_key;
ALTER TABLE hosts DROP COLUMN public_key;
ALTER TABLE hosts DROP COLUMN version;
