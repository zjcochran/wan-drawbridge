// SPDX-License-Identifier: AGPL-3.0-or-later

// Package store persists Drawbridge's state in SQLite.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/pressly/goose/v3"
	// The pure-Go SQLite driver, registered as "sqlite".
	_ "modernc.org/sqlite"

	"github.com/zjcochran/wan-drawbridge/internal/wan"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store holds all of Drawbridge's data in one SQLite database.
type Store struct {
	db      *sql.DB
	onEvent atomic.Pointer[func(Event)]
}

// OnEvent sets a function called with every event after it is recorded, for
// notifications. It must not block.
func (s *Store) OnEvent(fn func(Event)) { s.onEvent.Store(&fn) }

// Open opens (or creates) the database at path and applies migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	goose.SetBaseFS(migrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		db.Close()
		return nil, err
	}
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// WANState is the persisted state machine plus learning bookkeeping.
type WANState struct {
	Machine           wan.Machine
	Since             time.Time
	CurrentASN        int
	LearningStartedAt time.Time
}

// LoadState returns the saved state, or a fresh Learning state on first run.
func (s *Store) LoadState(ctx context.Context, now time.Time) (WANState, error) {
	var st WANState
	var state string
	var since, learning, pendingSince, lastEvidence, lastFailover, lastFailback int64
	var asn sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT state, since, current_asn, learning_started_at,
			pending_since, last_evidence, flaps, last_failover, last_failback
		FROM wan_state WHERE id = 1`).
		Scan(&state, &since, &asn, &learning, &pendingSince, &lastEvidence, &st.Machine.Flaps, &lastFailover, &lastFailback)
	if errors.Is(err, sql.ErrNoRows) {
		st = WANState{Machine: wan.Machine{State: wan.Learning}, Since: now, LearningStartedAt: now}
		return st, s.SaveState(ctx, st)
	}
	if err != nil {
		return st, err
	}
	st.Machine.State = wan.State(state)
	st.Machine.PendingSince, st.Machine.LastEvidence = fromUnix(pendingSince), fromUnix(lastEvidence)
	st.Machine.LastFailover, st.Machine.LastFailback = fromUnix(lastFailover), fromUnix(lastFailback)
	st.Since = time.Unix(since, 0)
	st.CurrentASN = int(asn.Int64)
	st.LearningStartedAt = time.Unix(learning, 0)
	return st, nil
}

// SaveState writes the WAN state, replacing the previous one.
func (s *Store) SaveState(ctx context.Context, st WANState) error {
	m := st.Machine
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO wan_state (id, state, since, current_asn, learning_started_at,
			pending_since, last_evidence, flaps, last_failover, last_failback)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET state = excluded.state, since = excluded.since,
			current_asn = excluded.current_asn, learning_started_at = excluded.learning_started_at,
			pending_since = excluded.pending_since, last_evidence = excluded.last_evidence, flaps = excluded.flaps,
			last_failover = excluded.last_failover, last_failback = excluded.last_failback`,
		string(m.State), st.Since.Unix(), nullInt(st.CurrentASN), st.LearningStartedAt.Unix(),
		toUnix(m.PendingSince), toUnix(m.LastEvidence), m.Flaps, toUnix(m.LastFailover), toUnix(m.LastFailback))
	return err
}

// toUnix and fromUnix store the zero time as 0.
func toUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// Check is one probe result as stored.
type Check struct {
	TS         time.Time
	IP         string
	ASN        int
	Conclusive bool
	Providers  any // marshalled to JSON
	LatencyMS  int64
}

// RecordCheck stores the result of one check.
func (s *Store) RecordCheck(ctx context.Context, c Check) error {
	providers, err := json.Marshal(c.Providers)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO checks (ts, ip, asn, conclusive, providers, latency_ms) VALUES (?, ?, ?, ?, ?, ?)`,
		c.TS.Unix(), nullStr(c.IP), nullInt(c.ASN), c.Conclusive, string(providers), c.LatencyMS)
	return err
}

// AddEvent appends to the audit log.
func (s *Store) AddEvent(ctx context.Context, now time.Time, typ string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO events (ts, type, payload) VALUES (?, ?, ?)`, now.Unix(), typ, string(b))
	if err != nil {
		return err
	}
	if fn := s.onEvent.Load(); fn != nil {
		id, _ := res.LastInsertId()
		var p map[string]any
		_ = json.Unmarshal(b, &p)
		(*fn)(Event{ID: id, TS: time.Unix(now.Unix(), 0), Type: typ, Payload: p})
	}
	return nil
}

// Prune deletes checks before checksBefore and daily totals before dailyBefore.
func (s *Store) Prune(ctx context.Context, checksBefore, dailyBefore time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM checks WHERE ts < ?`, checksBefore.Unix())
	if err != nil {
		return 0, err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM asn_daily WHERE day < ?`, day(dailyBefore)); err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func day(t time.Time) string { return t.UTC().Format(time.DateOnly) }

func nullInt(n int) sql.NullInt64 { return sql.NullInt64{Int64: int64(n), Valid: n != 0} }

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
