// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Row is one table row, column name to value, as it travels in a backup.
type Row map[string]any

// Snapshot is the data a backup carries: whole tables, row by row.
type Snapshot struct {
	Config  map[string][]Row `json:"config"`
	History map[string][]Row `json:"history,omitempty"` // nil: no history
}

// ConfigTables are copied whole, in an order that satisfies the foreign keys.
// Runtime state (wan_state, runs, run_actions, sessions) is never copied: it
// belongs to the running instance.
var ConfigTables = []string{"settings", "isps", "asns", "hosts", "stacks", "containers", "users", "oidc_identities", "recovery_codes"}

// historyTables are filtered by time: column holds a Unix time or, for
// asn_daily, a YYYY-MM-DD day.
var historyTables = []struct{ name, column string }{
	{"checks", "ts"},
	{"events", "ts"},
	{"asn_daily", "day"},
	{"outages", "started"},
	{"outage_usage", "ts"},
}

// ErrNoAdmin rejects a backup that would leave nobody able to manage Drawbridge.
var ErrNoAdmin = errors.New("the backup has no enabled admin account")

// SchemaVersion is the latest applied migration.
func (s *Store) SchemaVersion(ctx context.Context) (int64, error) {
	var v int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied`).Scan(&v)
	return v, err
}

// Export reads the configuration and, unless history is false, the history
// from since onwards (all of it when since is zero), in one consistent read.
func (s *Store) Export(ctx context.Context, history bool, since time.Time) (Snapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Snapshot{}, err
	}
	defer tx.Rollback() //nolint:errcheck // read-only
	snap := Snapshot{Config: map[string][]Row{}}
	for _, t := range ConfigTables {
		if snap.Config[t], err = dumpRows(ctx, tx, `SELECT * FROM `+t, nil); err != nil { //nolint:gosec // G202: fixed table names
			return Snapshot{}, fmt.Errorf("export %s: %w", t, err)
		}
	}
	if history {
		snap.History = map[string][]Row{}
		for _, h := range historyTables {
			var from any = since.Unix()
			if h.column == "day" {
				from = day(since)
			}
			if since.IsZero() {
				from = nil
			}
			q := `SELECT * FROM ` + h.name + ` WHERE ? IS NULL OR ` + h.column + ` >= ?` //nolint:gosec // G202: fixed names
			if snap.History[h.name], err = dumpRows(ctx, tx, q, []any{from, from}); err != nil {
				return Snapshot{}, fmt.Errorf("export %s: %w", h.name, err)
			}
		}
	}
	return snap, nil
}

func dumpRows(ctx context.Context, tx *sql.Tx, query string, args []any) ([]Row, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []Row{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		r := Row{}
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				vals[i] = string(b)
			}
			r[c] = vals[i]
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Import replaces the configuration with snap's, and the history too when
// snap has history, in one transaction: on any error nothing changes. Every
// session ends, because the users are replaced. Runtime state is kept.
func (s *Store) Import(ctx context.Context, snap Snapshot) error {
	for t := range snap.Config {
		if !slices.Contains(ConfigTables, t) {
			return fmt.Errorf("unknown table %q in the backup", t)
		}
	}
	for t := range snap.History {
		if !isHistoryTable(t) {
			return fmt.Errorf("unknown table %q in the backup", t)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	// Children first, so the foreign keys hold at every step.
	tables := []string{"sessions", "recovery_codes", "oidc_identities", "users", "asns", "isps", "stacks", "containers", "hosts", "settings"}
	if snap.History != nil {
		for _, h := range historyTables {
			tables = append(tables, h.name)
		}
	}
	for _, t := range tables {
		// Table names come from the fixed lists above, never from the backup.
		// nosemgrep
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t); err != nil { //nolint:gosec // G202: fixed table names
			return fmt.Errorf("clear %s: %w", t, err)
		}
	}
	for _, t := range ConfigTables {
		if err := loadRows(ctx, tx, t, snap.Config[t]); err != nil {
			return err
		}
	}
	for _, h := range historyTables {
		if err := loadRows(ctx, tx, h.name, snap.History[h.name]); err != nil {
			return err
		}
	}
	if err := upgradeOld(ctx, tx, snap); err != nil {
		return err
	}
	var admins int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = ? AND disabled = 0`, RoleAdmin).Scan(&admins); err != nil {
		return err
	}
	if admins == 0 {
		return ErrNoAdmin
	}
	return tx.Commit()
}

// upgradeOld fills in what a backup from an older version lacks.
func upgradeOld(ctx context.Context, tx *sql.Tx, snap Snapshot) error {
	if _, ok := snap.Config["hosts"]; !ok {
		// A backup from before hosts: everything in it is on the local host.
		if _, err := tx.ExecContext(ctx, `INSERT INTO hosts (id, name, kind, platform, status, created_at)
			VALUES (?, 'local', 'local', 'docker', 'online', unixepoch())`, LocalHost); err != nil {
			return err
		}
	}
	if _, ok := snap.History["outages"]; snap.History != nil && !ok {
		// A backup from before outages were recorded: recreate them from its events.
		return rebuildOutages(ctx, tx)
	}
	return nil
}

func loadRows(ctx context.Context, tx *sql.Tx, table string, rows []Row) error {
	if len(rows) == 0 {
		return nil
	}
	known, err := columns(ctx, tx, table)
	if err != nil {
		return err
	}
	for i, r := range rows {
		cols := make([]string, 0, len(r))
		args := make([]any, 0, len(r))
		for c, v := range r {
			if !known[c] {
				return fmt.Errorf("%s row %d: unknown column %q", table, i+1, c)
			}
			cols = append(cols, c)
			args = append(args, sqlValue(v))
		}
		// The table is from ConfigTables or historyTables, every column was
		// checked against the schema above, and values are bound arguments.
		q := `INSERT INTO ` + table + ` (` + strings.Join(cols, ", ") + `) VALUES (?` + strings.Repeat(", ?", len(cols)-1) + `)` //nolint:gosec // G202: names checked against the schema
		// nosemgrep
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("%s row %d: %w", table, i+1, err)
		}
	}
	return nil
}

// columns lists a table's columns, so only real column names reach the SQL.
func columns(ctx context.Context, tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// sqlValue turns a decoded JSON value into an SQLite one: whole numbers
// become integers, everything else is stored as it came.
func sqlValue(v any) any {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i
		}
		f, _ := n.Float64()
		return f
	case float64:
		if n == float64(int64(n)) {
			return int64(n)
		}
	case bool:
		if n {
			return int64(1)
		}
		return int64(0)
	}
	return v
}

func isHistoryTable(t string) bool {
	for _, h := range historyTables {
		if h.name == t {
			return true
		}
	}
	return false
}
