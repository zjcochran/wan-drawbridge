// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Run is one failover: the containers Drawbridge stopped and must start again.
type Run struct {
	ID        int64
	StartedAt time.Time
	Trigger   string
	ToASN     int
}

// RunAction is one container in a run.
type RunAction struct {
	ContainerID, Key, Name string
	WasRunning             bool
	Stopped, Started       bool
	Error                  string
}

// OpenRun returns the run that has not been closed yet, or nil.
func (s *Store) OpenRun(ctx context.Context) (*Run, error) {
	var r Run
	var started int64
	var asn sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, started_at, trigger, to_asn FROM runs WHERE status = 'open'`).Scan(&r.ID, &started, &r.Trigger, &asn)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.StartedAt, r.ToASN = time.Unix(started, 0), int(asn.Int64)
	return &r, nil
}

// CreateRun records the start of a container run.
func (s *Store) CreateRun(ctx context.Context, trigger string, toASN int, now time.Time) (*Run, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (started_at, trigger, to_asn) VALUES (?, ?, ?)`, now.Unix(), trigger, nullInt(toASN))
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Run{ID: id, StartedAt: now, Trigger: trigger, ToASN: toASN}, nil
}

// AddRunAction records a container in a run. A container already in the run
// keeps its original was_running, so a repeated stop pass cannot lose it.
func (s *Store) AddRunAction(ctx context.Context, runID int64, a RunAction) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_actions (run_id, container_id, container_key, name, was_running) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (run_id, container_id) DO NOTHING`,
		runID, a.ContainerID, a.Key, a.Name, a.WasRunning)
	return err
}

// RunActions returns a run's container actions, by container key.
func (s *Store) RunActions(ctx context.Context, runID int64) ([]RunAction, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT container_id, container_key, name, was_running, stopped_at IS NOT NULL, started_at IS NOT NULL, error
		FROM run_actions WHERE run_id = ? ORDER BY container_key`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunAction
	for rows.Next() {
		var a RunAction
		if err := rows.Scan(&a.ContainerID, &a.Key, &a.Name, &a.WasRunning, &a.Stopped, &a.Started, &a.Error); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkStopped records a stop attempt; a non-empty errMsg means it failed. A
// successful stop clears any earlier start, so a container started and then
// disabled again during the same failover is restarted at failback.
func (s *Store) MarkStopped(ctx context.Context, runID int64, containerID, errMsg string, now time.Time) error {
	q := `UPDATE run_actions SET error = ? WHERE run_id = ? AND container_id = ?`
	args := []any{errMsg, runID, containerID}
	if errMsg == "" {
		q = `UPDATE run_actions SET stopped_at = ?, started_at = NULL, error = '' WHERE run_id = ? AND container_id = ?`
		args = []any{now.Unix(), runID, containerID}
	}
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

// MarkStarted records a start attempt; a non-empty errMsg means it failed.
func (s *Store) MarkStarted(ctx context.Context, runID int64, containerID, errMsg string, now time.Time) error {
	q := `UPDATE run_actions SET error = ? WHERE run_id = ? AND container_id = ?`
	args := []any{errMsg, runID, containerID}
	if errMsg == "" {
		q = `UPDATE run_actions SET started_at = ?, error = '' WHERE run_id = ? AND container_id = ?`
		args = []any{now.Unix(), runID, containerID}
	}
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

// CloseRun ends a run with status "completed" or "failed".
func (s *Store) CloseRun(ctx context.Context, runID int64, status string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status = ?, ended_at = ? WHERE id = ?`, status, now.Unix(), runID)
	return err
}
