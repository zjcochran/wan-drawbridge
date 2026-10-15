// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/wan"
)

// Outage is one period in failover: from the failover until the failback.
type Outage struct {
	ID        int64
	Started   time.Time
	Ended     time.Time // zero while it lasts
	ISP       int       // the backup ISP, 0 when none was seen
	ISPName   string
	StartedBy string // the user of "Fail over now", "" when automatic
	EndedBy   string // the user of "Restore now", "" when automatic
	RX, TX    int64  // network bytes received and sent by the containers
	Measured  bool   // whether any container was measured
}

// Open reports whether the outage still lasts.
func (o Outage) Open() bool { return o.Ended.IsZero() }

// Duration is how long the outage lasted, or has lasted until now.
func (o Outage) Duration(now time.Time) time.Duration {
	end := o.Ended
	if o.Open() {
		end = now
	}
	return max(end.Sub(o.Started), 0)
}

// Usage is the network traffic of one container during outages.
type Usage struct {
	HostID    int64
	Host      string // the host's name
	Container string
	RX, TX    int64
}

// inOutage reports whether a state belongs to an outage: containers stay
// stopped until the failback.
func inOutage(s wan.State) bool { return s == wan.Failover || s == wan.FailbackPending }

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// syncOutage opens an outage when st is in failover and none is open, closes
// the open one when st is not, and notes the backup ISP once one is seen.
// st.Since is when the state was entered, so it dates both ends.
func syncOutage(ctx context.Context, db execer, st WANState) error {
	if !inOutage(st.Machine.State) {
		_, err := db.ExecContext(ctx, `UPDATE outages SET ended = MAX(started, ?) WHERE ended IS NULL`, st.Since.Unix())
		return err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO outages (started)
		SELECT ? WHERE NOT EXISTS (SELECT 1 FROM outages WHERE ended IS NULL)`, st.Since.Unix()); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `UPDATE outages SET isp_id = (
			SELECT a.isp_id FROM asns a JOIN isps i ON i.id = a.isp_id WHERE a.asn = ? AND i.role <> 'primary')
		WHERE ended IS NULL AND isp_id IS NULL`, st.CurrentASN)
	return err
}

// rebuildOutagesSQL recreates outages from the event log: a state change or
// manual failover into failover starts one, the next change out of failover
// or failback pending ends it.
const rebuildOutagesSQL = `
INSERT INTO outages (started, ended, isp_id, started_by, ended_by)
SELECT s.ts,
	COALESCE(e.ts, (SELECT MIN(y.ts) FROM events y WHERE y.id > s.id
		AND y.type IN ('state_changed', 'manual_failover') AND json_extract(y.payload, '$.to') = 'failover'
		AND COALESCE(json_extract(y.payload, '$.from'), '') NOT IN ('failover', 'failback_pending'))),
	NULLIF(json_extract(s.payload, '$.isp'), 0),
	CASE WHEN s.type = 'manual_failover' THEN COALESCE(json_extract(s.payload, '$.by'), '') ELSE '' END,
	CASE WHEN e.type = 'manual_restore' THEN COALESCE(json_extract(e.payload, '$.by'), '') ELSE '' END
FROM events s
LEFT JOIN events e ON e.id = (SELECT x.id FROM events x WHERE x.id > s.id
	AND x.type IN ('state_changed', 'manual_restore', 'manual_failover')
	AND json_extract(x.payload, '$.from') IN ('failover', 'failback_pending')
	AND COALESCE(json_extract(x.payload, '$.to'), '') NOT IN ('failover', 'failback_pending')
	ORDER BY x.id LIMIT 1)
WHERE s.type IN ('state_changed', 'manual_failover') AND json_extract(s.payload, '$.to') = 'failover'
	AND COALESCE(json_extract(s.payload, '$.from'), '') NOT IN ('failover', 'failback_pending')
ORDER BY s.id`

// rebuildOutages fills an empty outages table from the event log, for
// databases and backups from before outages were recorded.
func rebuildOutages(ctx context.Context, db execer) error {
	_, err := db.ExecContext(ctx, rebuildOutagesSQL)
	return err
}

// SetOutageBy records who started (start) or ended the latest outage by hand.
func (s *Store) SetOutageBy(ctx context.Context, start bool, by string) error {
	q := `UPDATE outages SET ended_by = ? WHERE id = (SELECT MAX(id) FROM outages)`
	if start {
		q = `UPDATE outages SET started_by = ? WHERE ended IS NULL`
	}
	_, err := s.db.ExecContext(ctx, q, by)
	return err
}

// OpenOutage returns the ID of the outage that lasts, 0 when there is none.
func (s *Store) OpenOutage(ctx context.Context) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM outages WHERE ended IS NULL`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// OutageFilter selects outages, newest first. Since keeps those that lasted
// past it; BeforeID pages backwards.
type OutageFilter struct {
	Since    time.Time
	BeforeID int64
	Limit    int // 0 for all
}

const outageQuery = `
SELECT o.id, o.started, COALESCE(o.ended, 0), COALESCE(o.isp_id, 0),
	COALESCE(NULLIF(i.display_name, ''), i.name, ''), o.started_by, o.ended_by,
	COALESCE(u.rx, 0), COALESCE(u.tx, 0), u.n IS NOT NULL
FROM outages o
LEFT JOIN isps i ON i.id = o.isp_id
LEFT JOIN (SELECT outage_id, SUM(rx) AS rx, SUM(tx) AS tx, COUNT(*) AS n FROM outage_usage GROUP BY outage_id) u
	ON u.outage_id = o.id`

// Outages returns the outages matching f, newest first.
func (s *Store) Outages(ctx context.Context, f OutageFilter) ([]Outage, error) {
	q := outageQuery + ` WHERE (o.ended IS NULL OR o.ended >= ?)`
	args := []any{f.Since.Unix()}
	if f.Since.IsZero() {
		args[0] = 0
	}
	if f.BeforeID > 0 {
		q += ` AND o.id < ?`
		args = append(args, f.BeforeID)
	}
	q += ` ORDER BY o.id DESC`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Outage
	for rows.Next() {
		o, err := scanOutage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// OutageByID returns one outage; ok is false when there is none.
func (s *Store) OutageByID(ctx context.Context, id int64) (o Outage, ok bool, err error) {
	o, err = scanOutage(s.db.QueryRowContext(ctx, outageQuery+` WHERE o.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return o, false, nil
	}
	return o, err == nil, err
}

func scanOutage(row interface{ Scan(...any) error }) (Outage, error) {
	var o Outage
	var started, ended int64
	err := row.Scan(&o.ID, &started, &ended, &o.ISP, &o.ISPName, &o.StartedBy, &o.EndedBy, &o.RX, &o.TX, &o.Measured)
	o.Started, o.Ended = time.Unix(started, 0), fromUnix(ended)
	return o, err
}

// OutageUsage returns each container's traffic during one outage, the
// biggest first.
func (s *Store) OutageUsage(ctx context.Context, id int64) ([]Usage, error) {
	return s.usage(ctx, `SELECT u.host_id, COALESCE(h.name, ''), u.container, u.rx, u.tx
		FROM outage_usage u LEFT JOIN hosts h ON h.id = u.host_id
		WHERE u.outage_id = ? ORDER BY u.rx + u.tx DESC, u.host_id, u.container`, id)
}

// UsageSince returns each container's traffic during the outages that lasted
// past since, the biggest first.
func (s *Store) UsageSince(ctx context.Context, since time.Time) ([]Usage, error) {
	return s.usage(ctx, `SELECT u.host_id, COALESCE(h.name, ''), u.container, SUM(u.rx), SUM(u.tx)
		FROM outage_usage u JOIN outages o ON o.id = u.outage_id LEFT JOIN hosts h ON h.id = u.host_id
		WHERE o.ended IS NULL OR o.ended >= ?
		GROUP BY u.host_id, u.container ORDER BY SUM(u.rx) + SUM(u.tx) DESC, u.host_id, u.container`, since.Unix())
}

func (s *Store) usage(ctx context.Context, q string, arg any) ([]Usage, error) {
	rows, err := s.db.QueryContext(ctx, q, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Usage
	for rows.Next() {
		var u Usage
		if err := rows.Scan(&u.HostID, &u.Host, &u.Container, &u.RX, &u.TX); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// RecordUsage adds a container's traffic since its last sample in the
// outage. rx and tx are Docker's counters, which count from the container's
// start: the first sample only sets the baseline, and when the container was
// re-created or restarted (new ID, or counters lower than before) the new
// counters count in full.
func (s *Store) RecordUsage(ctx context.Context, outageID, hostID int64, key, containerID string, rx, tx int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO outage_usage (outage_id, host_id, container, container_id, last_rx, last_tx, ts) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (outage_id, host_id, container) DO UPDATE SET
			rx = rx + CASE WHEN container_id = excluded.container_id AND excluded.last_rx >= last_rx AND excluded.last_tx >= last_tx
				THEN excluded.last_rx - last_rx ELSE excluded.last_rx END,
			tx = tx + CASE WHEN container_id = excluded.container_id AND excluded.last_rx >= last_rx AND excluded.last_tx >= last_tx
				THEN excluded.last_tx - last_tx ELSE excluded.last_tx END,
			container_id = excluded.container_id, last_rx = excluded.last_rx, last_tx = excluded.last_tx, ts = excluded.ts`,
		outageID, hostID, key, containerID, rx, tx, now.Unix())
	return err
}
