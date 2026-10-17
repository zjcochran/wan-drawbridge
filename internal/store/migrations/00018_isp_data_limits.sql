-- +goose Up
-- Data limits per ISP, in bytes; 0 is off. An outage on the ISP sends a
-- data_alert when the containers' traffic passes one. They replace the one
-- global data_alerts setting, whose values go to every backup ISP.
ALTER TABLE isps ADD COLUMN limit_outage_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE isps ADD COLUMN limit_month_bytes INTEGER NOT NULL DEFAULT 0;
UPDATE isps SET
	limit_outage_bytes = COALESCE((SELECT json_extract(value, '$.outage_bytes') FROM settings WHERE key = 'data_alerts'), 0),
	limit_month_bytes = COALESCE((SELECT json_extract(value, '$.month_bytes') FROM settings WHERE key = 'data_alerts'), 0)
WHERE role = 'backup';
DELETE FROM settings WHERE key IN ('data_alerts', 'data_alerts_sent');

-- +goose Down
ALTER TABLE isps DROP COLUMN limit_month_bytes;
ALTER TABLE isps DROP COLUMN limit_outage_bytes;
