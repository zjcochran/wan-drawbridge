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
