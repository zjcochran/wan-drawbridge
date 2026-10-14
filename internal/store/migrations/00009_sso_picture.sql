-- +goose Up
-- Whether single sign-on may set the user's picture. Removing a picture that
-- came from single sign-on turns it off, so the next sign-in does not bring
-- it back.
ALTER TABLE users ADD COLUMN sso_picture INTEGER NOT NULL DEFAULT 1;

-- +goose Down
ALTER TABLE users DROP COLUMN sso_picture;
