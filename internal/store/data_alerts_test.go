// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

func TestDataLimits(t *testing.T) {
	ctx, s := context.Background(), open(t)
	isp, _, _ := s.SeeASN(ctx, 64510, "SKYLINK-AS64510", t0)
	if err := s.SetDataLimits(ctx, isp, DataLimits{OutageBytes: 5e9, MonthBytes: -1}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDataLimits(ctx, 999, DataLimits{}); err == nil {
		t.Error("limits for an unknown ISP were saved")
	}
	rows, _ := s.ISPs(ctx, t0)
	if len(rows) != 1 || rows[0].Limits != (DataLimits{OutageBytes: 5e9}) {
		t.Errorf("limits = %+v", rows)
	}
	if got, l, err := s.OutageLimits(ctx, 42); got != 0 || l != (DataLimits{}) || err != nil {
		t.Errorf("no outage: %d %+v %v", got, l, err)
	}
}

// TestGlobalDataLimitsMigrate checks that migration 18 gives the global
// limits to every backup ISP and drops the old settings.
func TestGlobalDataLimitsMigrate(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(migrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(ctx, db, "migrations", 17); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO isps (id, name, group_key, role, first_seen, last_seen) VALUES (1, 'EXAMPLENET', 'EXAMPLENET', 'primary', 1, 1)`,
		`INSERT INTO isps (id, name, group_key, role, first_seen, last_seen) VALUES (2, 'SKYLINK', 'SKYLINK', 'backup', 1, 1)`,
		`INSERT INTO settings (key, value) VALUES ('data_alerts', '{"outage_bytes":5000000000,"month_bytes":0}')`,
		`INSERT INTO settings (key, value) VALUES ('data_alerts_sent', '{"outage":3,"month":"2026-10"}')`,
	} {
		// Fixed statements from the list above, no input: test data for an old schema.
		// nosemgrep
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	db.Close()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, _ := s.ISPs(ctx, t0)
	for _, r := range rows {
		want := DataLimits{}
		if r.ID == 2 {
			want.OutageBytes = 5e9
		}
		if r.Limits != want {
			t.Errorf("ISP %d limits = %+v", r.ID, r.Limits)
		}
	}
	var v any
	if ok, _ := s.GetSetting(ctx, "data_alerts", &v); ok {
		t.Error("the global setting is still there")
	}
}

// TestRestoreGlobalDataLimits restores a backup made while the limits were
// global.
func TestRestoreGlobalDataLimits(t *testing.T) {
	ctx, s := context.Background(), open(t)
	isp, _, _ := s.SeeASN(ctx, 64510, "SKYLINK-AS64510", t0)
	if _, err := s.CreateUser(ctx, "alice", "test-password-123", RoleAdmin, t0); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Export(ctx, false, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range snap.Config["isps"] {
		delete(r, "limit_outage_bytes")
		delete(r, "limit_month_bytes")
	}
	snap.Config["settings"] = append(snap.Config["settings"], Row{"key": "data_alerts", "value": `{"outage_bytes":0,"month_bytes":7000000000}`})
	if err := s.Import(ctx, snap); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.ISPs(ctx, t0)
	if len(rows) != 1 || rows[0].ID != isp || rows[0].Limits != (DataLimits{MonthBytes: 7e9}) {
		t.Errorf("limits after the restore = %+v", rows)
	}
}

// TestPrimaryMetered checks that the primary's switch is its own: off by
// default, and turning it on or off leaves the backup flag alone.
func TestPrimaryMetered(t *testing.T) {
	ctx, s := context.Background(), open(t)
	isp, _, _ := s.SeeASN(ctx, 64500, "EXAMPLENET-64500", t0)
	if err := s.SetRole(ctx, isp, RolePrimary, true); err != nil {
		t.Fatal(err)
	}
	metered := func() bool {
		rows, _ := s.ISPs(ctx, t0)
		return rows[0].Metered
	}
	if metered() {
		t.Error("a new primary is metered")
	}
	s.SetMetered(ctx, isp, true)
	if !metered() {
		t.Error("the primary's switch did not turn on")
	}
	s.SetMetered(ctx, isp, false)
	if m, _ := s.Metered(ctx, isp); !m {
		t.Error("the primary's switch changed the backup flag")
	}
	s.SetRole(ctx, isp, RoleBackup, true)
	if !metered() {
		t.Error("the ISP is no longer a metered backup")
	}
}

// at19 opens a store made at migration 19 with the given rows, migrating it
// to the current schema.
func at19(t *testing.T, rows ...string) *Store {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(migrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(ctx, db, "migrations", 19); err != nil {
		t.Fatal(err)
	}
	for _, q := range rows {
		// Fixed statements from the tests below, no input: test data for an old schema.
		// nosemgrep
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	db.Close()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestPrimaryMeteredMigrate checks migration 20: a primary whose backup flag
// was turned off on the ISPs page since migration 19 gets it back on; one
// unmetered before that keeps its setting.
func TestPrimaryMeteredMigrate(t *testing.T) {
	ctx := context.Background()
	const primary = `INSERT INTO isps (id, name, group_key, role, metered, first_seen, last_seen) VALUES (1, 'EXAMPLENET', 'EXAMPLENET', 'primary', 0, 1, 1)`
	s := at19(t, primary, `INSERT INTO events (ts, type, payload) VALUES (unixepoch() + 5, 'isp_metered', '{"isp":1,"metered":false,"by":"alice"}')`)
	if m, _ := s.Metered(ctx, 1); !m {
		t.Error("the primary turned off since migration 19 is still an unmetered backup")
	}
	s = at19(t, primary, `INSERT INTO events (ts, type, payload) VALUES (1, 'isp_metered', '{"isp":1,"metered":false,"by":"alice"}')`)
	if m, _ := s.Metered(ctx, 1); m {
		t.Error("a primary unmetered before migration 19 was changed")
	}
}
