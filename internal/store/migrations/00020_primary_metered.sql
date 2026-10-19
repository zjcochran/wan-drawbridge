-- +goose Up
-- The primary's own Metered switch: whether its traffic counts towards its
-- monthly data limit. Off by default. Before this, the primary showed the
-- backup switch (metered, on by default), and turning it off would have made
-- the ISP an unmetered backup if it ever became one. So a primary whose
-- metered flag was last turned off on the ISPs page since migration 19 gets
-- it back on: that change was meant for the primary only.
ALTER TABLE isps ADD COLUMN primary_metered INTEGER NOT NULL DEFAULT 0;
UPDATE isps SET metered = 1
WHERE role = 'primary' AND metered = 0 AND (
	SELECT json_extract(e.payload, '$.metered') FROM events e
	WHERE e.type = 'isp_metered' AND json_extract(e.payload, '$.isp') = isps.id
	ORDER BY e.id DESC LIMIT 1
) = 0 AND (
	SELECT MAX(e.ts) FROM events e
	WHERE e.type = 'isp_metered' AND json_extract(e.payload, '$.isp') = isps.id
) >= (SELECT unixepoch(tstamp) FROM goose_db_version WHERE version_id = 19 AND is_applied ORDER BY id DESC LIMIT 1);

-- +goose Down
ALTER TABLE isps DROP COLUMN primary_metered;
