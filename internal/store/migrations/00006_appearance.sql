-- +goose Up
-- Each user picks a colour theme and light, dark or automatic mode.
ALTER TABLE users ADD COLUMN theme TEXT NOT NULL DEFAULT 'default';
ALTER TABLE users ADD COLUMN mode TEXT NOT NULL DEFAULT 'auto';

-- +goose Down
ALTER TABLE users DROP COLUMN mode;
ALTER TABLE users DROP COLUMN theme;
