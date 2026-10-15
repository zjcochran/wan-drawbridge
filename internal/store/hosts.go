// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"database/sql"
	"time"
)

// LocalHost is the ID of the local Docker host, which always exists.
const LocalHost int64 = 1

// Host statuses.
const (
	HostPending  = "pending"
	HostOnline   = "online"
	HostOffline  = "offline"
	HostRevoked  = "revoked"
	HostDisabled = "disabled"
)

// HostRow is one host Drawbridge controls.
type HostRow struct {
	ID        int64
	Name      string
	Kind      string // "local" or "agent"
	Platform  string // "docker", ...
	Status    string
	LastSeen  time.Time // zero if never
	LastError string
}

// Hosts returns every host, the local host first.
func (s *Store) Hosts(ctx context.Context) ([]HostRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, kind, platform, status, last_seen, last_error FROM hosts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HostRow
	for rows.Next() {
		var h HostRow
		var seen sql.NullInt64
		if err := rows.Scan(&h.ID, &h.Name, &h.Kind, &h.Platform, &h.Status, &seen, &h.LastError); err != nil {
			return nil, err
		}
		if seen.Valid {
			h.LastSeen = time.Unix(seen.Int64, 0)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// HostNames maps host IDs to names.
func (s *Store) HostNames(ctx context.Context) (map[int64]string, error) {
	hs, err := s.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]string, len(hs))
	for _, h := range hs {
		out[h.ID] = h.Name
	}
	return out, nil
}

// AddHost records a new host and returns its ID.
func (s *Store) AddHost(ctx context.Context, name, kind, platform, actor string, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO hosts (name, kind, platform, created_at, created_by) VALUES (?, ?, ?, ?, ?)`,
		name, kind, platform, now.Unix(), actor)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetHostReached records that a host answered (errMsg empty) or could not
// be reached. It changes nothing for a revoked or disabled host.
func (s *Store) SetHostReached(ctx context.Context, id int64, errMsg string, now time.Time) error {
	if errMsg == "" {
		_, err := s.db.ExecContext(ctx, `
			UPDATE hosts SET status = 'online', last_seen = ?, last_error = ''
			WHERE id = ? AND status NOT IN ('revoked', 'disabled')`, now.Unix(), id)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE hosts SET status = 'offline', last_error = ?
		WHERE id = ? AND status NOT IN ('revoked', 'disabled')`, errMsg, id)
	return err
}
