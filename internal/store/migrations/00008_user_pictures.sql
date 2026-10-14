-- +goose Up
-- Profile pictures: uploaded by the user, or fetched from single sign-on at
-- sign-in. An uploaded picture is never replaced by the provider's.
CREATE TABLE user_pictures (
    user_id      INTEGER PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    content_type TEXT    NOT NULL,
    data         BLOB    NOT NULL,
    source       TEXT    NOT NULL CHECK (source IN ('upload', 'sso')),
    updated_at   INTEGER NOT NULL
);

-- +goose Down
DROP TABLE user_pictures;
