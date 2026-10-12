-- +goose Up

CREATE TABLE oidc_identities (
    issuer    TEXT    NOT NULL,
    subject   TEXT    NOT NULL,
    user_id   INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    email     TEXT    NOT NULL DEFAULT '',
    linked_at INTEGER NOT NULL,
    PRIMARY KEY (issuer, subject)
);
CREATE INDEX oidc_identities_user ON oidc_identities (user_id);

-- +goose Down
DROP TABLE oidc_identities;
