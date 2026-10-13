// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"encoding/json"
	"time"
)

// Event is one audit log entry.
type Event struct {
	ID      int64
	TS      time.Time
	Type    string
	Actor   string
	Payload map[string]any
}

// EventFilter selects events, newest first. BeforeID pages backwards.
type EventFilter struct {
	Type     string
	BeforeID int64
	Limit    int
}

// Events returns the events matching f, newest first: f.Limit of them
// (default 100, at most 1000).
func (s *Store) Events(ctx context.Context, f EventFilter) ([]Event, error) {
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 100
	}
	q := `SELECT id, ts, type, actor, payload FROM events WHERE 1 = 1`
	var args []any
	if f.Type != "" {
		q += ` AND type = ?`
		args = append(args, f.Type)
	}
	if f.BeforeID > 0 {
		q += ` AND id < ?`
		args = append(args, f.BeforeID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var ts int64
		var payload string
		if err := rows.Scan(&e.ID, &ts, &e.Type, &e.Actor, &payload); err != nil {
			return nil, err
		}
		e.TS = time.Unix(ts, 0)
		_ = json.Unmarshal([]byte(payload), &e.Payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

// EventTypes lists the event types present, for the filter.
func (s *Store) EventTypes(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT type FROM events ORDER BY type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CheckPoint is one check for the dashboard timeline.
type CheckPoint struct {
	TS         time.Time
	ISP        int // 0 when inconclusive or unknown
	Conclusive bool
}

// ChecksSince returns checks after since, oldest first, mapped to ISP groups.
func (s *Store) ChecksSince(ctx context.Context, since time.Time) ([]CheckPoint, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.ts, COALESCE(a.isp_id, 0), c.conclusive FROM checks c
		LEFT JOIN asns a ON a.asn = c.asn
		WHERE c.ts >= ? ORDER BY c.ts`, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CheckPoint
	for rows.Next() {
		var p CheckPoint
		var ts int64
		if err := rows.Scan(&ts, &p.ISP, &p.Conclusive); err != nil {
			return nil, err
		}
		p.TS = time.Unix(ts, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}
