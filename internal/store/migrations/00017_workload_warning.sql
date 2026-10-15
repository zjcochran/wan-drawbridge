-- +goose Up
-- Why stopping a workload may not last, as its host reports it (for example
-- an autoscaler that scales it back up); '' when nothing is known.
ALTER TABLE containers ADD COLUMN warning TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE containers DROP COLUMN warning;
