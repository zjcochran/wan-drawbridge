-- +goose Up
-- Tokens for the JSON API. Only a SHA-256 hash of each token is kept; the
-- token itself is shown once, when it is made. Not in backup files.
CREATE TABLE api_tokens (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL,
    hash       TEXT    NOT NULL UNIQUE,
    created_at INTEGER NOT NULL,
    created_by TEXT    NOT NULL DEFAULT '',
    last_used  INTEGER NOT NULL DEFAULT 0
);

-- +goose Down
DROP TABLE api_tokens;
