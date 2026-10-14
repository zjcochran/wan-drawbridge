-- +goose Up
-- Two-factor sign-in for local passwords. totp_secret is sealed with the
-- instance key ('' = off); totp_step is the last time step accepted, so a
-- code cannot be used twice. Recovery codes are stored as SHA-256 hashes and
-- deleted when used.
ALTER TABLE users ADD COLUMN totp_secret TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN totp_step INTEGER NOT NULL DEFAULT 0;

CREATE TABLE recovery_codes (
    user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    hash    TEXT    NOT NULL,
    PRIMARY KEY (user_id, hash)
);

-- +goose Down
DROP TABLE recovery_codes;
ALTER TABLE users DROP COLUMN totp_step;
ALTER TABLE users DROP COLUMN totp_secret;
