// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fill gives a store one of everything a backup carries.
func fill(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	isp, _, err := s.SeeASN(ctx, 64500, "EXAMPLENET-64500", t0)
	must(err)
	_, _, err = s.SeeASN(ctx, 64510, "SKYLINK-64510", t0)
	must(err)
	must(s.SetRole(ctx, isp, RolePrimary, true))
	must(s.SetSetting(ctx, SettingDetection, map[string]any{"quorum": 2}))
	must(s.SetSetting(ctx, SettingSetupComplete, true))
	_, err = s.SyncInventory(ctx, LocalHost, []InventoryItem{{Key: "media/player", Project: "media", Service: "player", Name: "media-player-1"}}, t0)
	must(err)
	must(s.SetStackPolicy(ctx, LocalHost, "media", true, "alice", t0))
	u, err := s.CreateUser(ctx, "alice", "test-password-123", RoleAdmin, t0)
	must(err)
	_, err = s.CreateUser(ctx, "viewer", "test-password-123", RoleViewer, t0)
	must(err)
	must(s.LinkIdentity(ctx, u.ID, "https://id.example.com", "sub-1", "alice@example.com", t0))
	for i := range 3 { // one check a day, 0-2 days old
		ts := t0.AddDate(0, 0, -i)
		must(s.RecordCheck(ctx, Check{TS: ts, IP: "203.0.113.7", ASN: 64500, Conclusive: true, Providers: []string{"a"}, LatencyMS: 12}))
		must(s.AddEvent(ctx, ts, "state_changed", map[string]any{"to": "primary"}))
		must(s.Observe(ctx, 64500, 60, ts))
	}
}

func count(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	// Tests pass fixed table names.
	// nosemgrep
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// viaJSON sends a snapshot through JSON the way a backup file does.
func viaJSON(t *testing.T, snap Snapshot) Snapshot {
	t.Helper()
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	var out Snapshot
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestExportImportConfig(t *testing.T) {
	ctx := context.Background()
	src, dst := open(t), open(t)
	fill(t, src)
	snap, err := src.Export(ctx, false, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if snap.History != nil {
		t.Fatal("history exported without being asked for")
	}

	// dst has its own config, a session and history that must survive.
	if _, err := dst.CreateUser(ctx, "admin", "test-password-123", RoleAdmin, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.CreateSession(ctx, 1, "192.0.2.1", "test", t0); err != nil {
		t.Fatal(err)
	}
	if err := dst.AddEvent(ctx, t0, "relearn", nil); err != nil {
		t.Fatal(err)
	}
	if err := dst.Import(ctx, viaJSON(t, snap)); err != nil {
		t.Fatal(err)
	}

	got, err := dst.Export(ctx, false, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Config, snap.Config) {
		t.Fatalf("config differs after the round trip:\n got %v\nwant %v", got.Config, snap.Config)
	}
	if count(t, dst, "sessions") != 0 {
		t.Error("sessions survived the restore")
	}
	if count(t, dst, "events") != 1 || count(t, dst, "checks") != 0 {
		t.Error("history changed although the backup had none")
	}
	if _, err := dst.Authenticate(ctx, "alice", "test-password-123", t0); err != nil {
		t.Errorf("restored user cannot sign in: %v", err)
	}
	if id, pinned, _ := dst.Primary(ctx); id == 0 || !pinned {
		t.Error("primary ISP not restored")
	}
}

func TestExportHistoryRange(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	fill(t, s)
	for since, want := range map[time.Time]int{
		{}:                      3,
		t0.AddDate(0, 0, -1):    2,
		t0.Add(-time.Hour):      1,
		t0.AddDate(0, 0, 1):     0,
		t0.AddDate(0, 0, -1000): 3,
	} {
		snap, err := s.Export(ctx, true, since)
		if err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"checks", "events", "asn_daily"} {
			if n := len(withoutNewISP(snap.History[table])); n != want {
				t.Errorf("since %v: %d %s, want %d", since, n, table, want)
			}
		}
	}
}

func TestImportReplacesHistory(t *testing.T) {
	ctx := context.Background()
	src, dst := open(t), open(t)
	fill(t, src)
	snap, err := src.Export(ctx, true, t0.AddDate(0, 0, -1))
	if err != nil {
		t.Fatal(err)
	}
	fill(t, dst)
	if err := dst.Import(ctx, viaJSON(t, snap)); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"checks", "events", "asn_daily"} {
		if n := count(t, dst, table); n != len(snap.History[table]) || len(withoutNewISP(snap.History[table])) != 2 {
			t.Errorf("%s: %d rows, want the backup's %d", table, n, len(snap.History[table]))
		}
	}
}

func TestImportRejectsBadBackups(t *testing.T) {
	ctx := context.Background()
	src := open(t)
	fill(t, src)
	good, err := src.Export(ctx, true, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	edit := func(f func(*Snapshot)) Snapshot {
		s := viaJSON(t, good)
		f(&s)
		return s
	}
	cases := map[string]Snapshot{
		"unknown table":   edit(func(s *Snapshot) { s.Config["sessions"] = []Row{} }),
		"unknown history": edit(func(s *Snapshot) { s.History["runs"] = []Row{} }),
		"unknown column":  edit(func(s *Snapshot) { s.Config["stacks"][0]["evil; DROP TABLE users"] = 1 }),
		"no admin":        edit(func(s *Snapshot) { s.Config["users"] = s.Config["users"][1:]; s.Config["oidc_identities"] = nil }),
		"broken reference": edit(func(s *Snapshot) {
			s.Config["asns"][0]["isp_id"] = json.Number("99")
		}),
	}
	for name, snap := range cases {
		dst := open(t)
		fill(t, dst)
		before, _ := dst.Export(ctx, true, time.Time{})
		err := dst.Import(ctx, snap)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if name == "no admin" && !errors.Is(err, ErrNoAdmin) {
			t.Errorf("no admin: %v", err)
		}
		after, _ := dst.Export(ctx, true, time.Time{})
		if !reflect.DeepEqual(before, after) {
			t.Errorf("%s: a rejected backup changed the data", name)
		}
	}
}

func TestSQLValue(t *testing.T) {
	for in, want := range map[any]any{
		json.Number("42"): int64(42), json.Number("1.5"): 1.5, 3.0: int64(3), 2.5: 2.5,
		true: int64(1), false: int64(0), "text": "text",
	} {
		if got := sqlValue(in); got != want {
			t.Errorf("sqlValue(%v) = %v (%T), want %v", in, got, got, want)
		}
	}
	if sqlValue(nil) != nil {
		t.Error("nil")
	}
}

func TestSchemaVersion(t *testing.T) {
	v, err := open(t).SchemaVersion(context.Background())
	if err != nil || v < 5 {
		t.Fatalf("schema version %d, %v", v, err)
	}
}

// withoutNewISP drops the isp_new events that fill records when it first sees
// each ISP, leaving the one-a-day rows the range tests count.
func withoutNewISP(rows []Row) []Row {
	var out []Row
	for _, r := range rows {
		if r["type"] != "isp_new" {
			out = append(out, r)
		}
	}
	return out
}

// A backup from before hosts has no hosts table and no host_id columns:
// everything in it lands on the local host.
func TestImportBackupWithoutHosts(t *testing.T) {
	ctx := context.Background()
	src, dst := open(t), open(t)
	fill(t, src)
	snap, err := src.Export(ctx, false, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	delete(snap.Config, "hosts")
	for _, table := range []string{"stacks", "containers"} {
		for _, r := range snap.Config[table] {
			delete(r, "host_id")
		}
	}
	if err := dst.Import(ctx, viaJSON(t, snap)); err != nil {
		t.Fatal(err)
	}
	hs, err := dst.Hosts(ctx)
	if err != nil || len(hs) != 1 || hs[0].ID != LocalHost || hs[0].Name != "local" {
		t.Fatalf("hosts = %+v, %v", hs, err)
	}
	stacks, _ := dst.Stacks(ctx)
	if len(stacks) != 1 || stacks[0].HostID != LocalHost || !stacks[0].Disabled || stacks[0].Containers != 1 {
		t.Errorf("stacks = %+v", stacks)
	}
}
