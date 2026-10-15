-- +goose Up
-- What a container row is: a Docker container, or a Swarm service.
ALTER TABLE containers ADD COLUMN kind TEXT NOT NULL DEFAULT 'container';

-- +goose Down
ALTER TABLE containers DROP COLUMN kind;
