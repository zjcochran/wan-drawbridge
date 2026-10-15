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
	HostID                 int64
	Host                   string // the host's name
	ContainerID, Key, Name string
	WasRunning             bool
	Stopped, Started       bool
	Error                  string
	Restore                []byte // what starting it again needs; empty for Docker containers
	Pending                bool   // a start owed to a host that could not be reached
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
		INSERT INTO run_actions (run_id, host_id, container_id, container_key, name, was_running) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (run_id, host_id, container_id) DO NOTHING`,
		runID, a.HostID, a.ContainerID, a.Key, a.Name, a.WasRunning)
	return err
}

const runActionColumns = `a.run_id, a.host_id, COALESCE(h.name, ''), a.container_id, a.container_key, a.name, a.was_running,
	a.stopped_at IS NOT NULL, a.started_at IS NOT NULL, a.error, a.restore, a.pending
	FROM run_actions a LEFT JOIN hosts h ON h.id = a.host_id`

func scanRunActions(rows *sql.Rows) (runIDs []int64, out []RunAction, err error) {
	defer rows.Close()
	for rows.Next() {
		var a RunAction
		var runID int64
		var restore string
		if err := rows.Scan(&runID, &a.HostID, &a.Host, &a.ContainerID, &a.Key, &a.Name, &a.WasRunning,
			&a.Stopped, &a.Started, &a.Error, &restore, &a.Pending); err != nil {
			return nil, nil, err
		}
		if restore != "" {
			a.Restore = []byte(restore)
		}
		runIDs = append(runIDs, runID)
		out = append(out, a)
	}
	return runIDs, out, rows.Err()
}

// RunActions returns a run's container actions, by host then container key.
func (s *Store) RunActions(ctx context.Context, runID int64) ([]RunAction, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runActionColumns+`
		WHERE a.run_id = ? ORDER BY a.host_id, a.container_key`, runID)
	if err != nil {
		return nil, err
	}
	_, out, err := scanRunActions(rows)
	return out, err
}

// PendingStart is a start owed by an ended run.
type PendingStart struct {
	RunID int64
	RunAction
}

// PendingStarts returns the starts owed to hosts that could not be reached
// when their run ended, oldest run first.
func (s *Store) PendingStarts(ctx context.Context) ([]PendingStart, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runActionColumns+`
		WHERE a.pending = 1 ORDER BY a.run_id, a.host_id, a.container_key`)
	if err != nil {
		return nil, err
	}
	ids, as, err := scanRunActions(rows)
	out := make([]PendingStart, len(as))
	for i := range as {
		out[i] = PendingStart{ids[i], as[i]}
	}
	return out, err
}

// TakePendingStarts moves the starts still owed by earlier runs into a new
// run, as containers that were running: a failover that begins before a host
// came back starts them with everything else at its failback.
func (s *Store) TakePendingStarts(ctx context.Context, runID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO run_actions (run_id, host_id, container_id, container_key, name, was_running, stopped_at, restore)
		SELECT ?, host_id, container_id, container_key, name, 1, stopped_at, restore FROM run_actions
		WHERE pending = 1 AND run_id <> ?
		ON CONFLICT (run_id, host_id, container_id) DO UPDATE SET was_running = 1, stopped_at = excluded.stopped_at, restore = excluded.restore`,
		runID, runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE run_actions SET pending = 0 WHERE pending = 1 AND run_id <> ?`, runID); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkStopped records a stop attempt; a non-empty errMsg means it failed. A
// successful stop clears any earlier start, so a container started and then
// disabled again during the same failover is restarted at failback, and keeps
// what starting it again needs.
func (s *Store) MarkStopped(ctx context.Context, runID, hostID int64, containerID string, restore []byte, errMsg string, now time.Time) error {
	q := `UPDATE run_actions SET error = ? WHERE run_id = ? AND host_id = ? AND container_id = ?`
	args := []any{errMsg, runID, hostID, containerID}
	if errMsg == "" {
		q = `UPDATE run_actions SET stopped_at = ?, started_at = NULL, error = '', restore = ?
			WHERE run_id = ? AND host_id = ? AND container_id = ?`
		args = []any{now.Unix(), string(restore), runID, hostID, containerID}
	}
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

// MarkStarted records a start attempt; a non-empty errMsg means it failed.
// Either way the start is no longer pending.
func (s *Store) MarkStarted(ctx context.Context, runID, hostID int64, containerID, errMsg string, now time.Time) error {
	q := `UPDATE run_actions SET error = ?, pending = 0 WHERE run_id = ? AND host_id = ? AND container_id = ?`
	args := []any{errMsg, runID, hostID, containerID}
	if errMsg == "" {
		q = `UPDATE run_actions SET started_at = ?, error = '', pending = 0 WHERE run_id = ? AND host_id = ? AND container_id = ?`
		args = []any{now.Unix(), runID, hostID, containerID}
	}
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

// MarkPending records that a container could not be started because its host
// could not be reached; it is started when the host is back.
func (s *Store) MarkPending(ctx context.Context, runID, hostID int64, containerID, errMsg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE run_actions SET pending = 1, error = ? WHERE run_id = ? AND host_id = ? AND container_id = ?`,
		errMsg, runID, hostID, containerID)
	return err
}

// CloseRun ends a run with status "completed" or "failed".
func (s *Store) CloseRun(ctx context.Context, runID int64, status string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status = ?, ended_at = ? WHERE id = ?`, status, now.Unix(), runID)
	return err
}
