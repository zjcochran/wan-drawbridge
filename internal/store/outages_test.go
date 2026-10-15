// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/wan"
)

func saveState(t *testing.T, s *Store, state wan.State, since time.Time, asn int) {
	t.Helper()
	st := WANState{Machine: wan.Machine{State: state}, Since: since, CurrentASN: asn, LearningStartedAt: t0}
	if err := s.SaveState(context.Background(), st); err != nil {
		t.Fatal(err)
	}
}

func TestOutagesFollowTheState(t *testing.T) {
	ctx, s := context.Background(), open(t)
	primary, _, _ := s.SeeASN(ctx, 64500, "EXAMPLENET-64500", t0)
	if err := s.SetRole(ctx, primary, RolePrimary, true); err != nil {
		t.Fatal(err)
	}
	backup, _, _ := s.SeeASN(ctx, 64510, "SKYLINK-64510", t0)

	saveState(t, s, wan.Primary, t0, 64500)
	if id, err := s.OpenOutage(ctx); err != nil || id != 0 {
		t.Fatalf("open outage on the primary = %d, %v", id, err)
	}
	// A manual failover while still on the primary: no backup ISP yet.
	saveState(t, s, wan.Failover, t0.Add(time.Hour), 64500)
	if err := s.SetOutageBy(ctx, true, "alice"); err != nil {
		t.Fatal(err)
	}
	id, _ := s.OpenOutage(ctx)
	o, ok, err := s.OutageByID(ctx, id)
	if err != nil || !ok || !o.Open() || o.ISP != 0 || o.StartedBy != "alice" || !o.Started.Equal(t0.Add(time.Hour)) {
		t.Fatalf("outage = %+v, %v, %v", o, ok, err)
	}
	// The backup shows up, then failback pending keeps the outage open.
	saveState(t, s, wan.Failover, t0.Add(time.Hour), 64510)
	saveState(t, s, wan.FailbackPending, t0.Add(2*time.Hour), 64500)
	saveState(t, s, wan.Primary, t0.Add(3*time.Hour), 64500)
	if err := s.SetOutageBy(ctx, false, "bob"); err != nil {
		t.Fatal(err)
	}
	o, _, _ = s.OutageByID(ctx, id)
	if o.Open() || o.ISP != backup || o.ISPName != "SKYLINK" || o.EndedBy != "bob" || o.Duration(t0) != 2*time.Hour {
		t.Fatalf("closed outage = %+v", o)
	}
	if id, _ := s.OpenOutage(ctx); id != 0 {
		t.Fatalf("still open: %d", id)
	}
	if _, ok, err := s.OutageByID(ctx, 99); ok || err != nil {
		t.Errorf("missing outage = %v, %v", ok, err)
	}
}

func TestOutageQueries(t *testing.T) {
	ctx, s := context.Background(), open(t)
	for i := range 3 { // outages starting 30, 20 and 10 days ago, an hour each
		start := t0.AddDate(0, 0, -30+10*i)
		saveState(t, s, wan.Failover, start, 0)
		saveState(t, s, wan.Primary, start.Add(time.Hour), 0)
	}
	saveState(t, s, wan.Failover, t0, 0) // ongoing
	all, err := s.Outages(ctx, OutageFilter{})
	if err != nil || len(all) != 4 || !all[0].Open() || all[0].Duration(t0.Add(time.Minute)) != time.Minute {
		t.Fatalf("all = %+v, %v", all, err)
	}
	recent, _ := s.Outages(ctx, OutageFilter{Since: t0.AddDate(0, 0, -15)})
	if len(recent) != 2 {
		t.Errorf("since 15 days = %d outages, want 2", len(recent))
	}
	page, _ := s.Outages(ctx, OutageFilter{BeforeID: all[1].ID, Limit: 1})
	if len(page) != 1 || page[0].ID != all[2].ID {
		t.Errorf("page = %+v", page)
	}
}

func TestRecordUsage(t *testing.T) {
	ctx, s := context.Background(), open(t)
	saveState(t, s, wan.Failover, t0, 0)
	id, _ := s.OpenOutage(ctx)
	rec := func(key, cid string, rx, tx int64) {
		t.Helper()
		if err := s.RecordUsage(ctx, id, LocalHost, key, cid, rx, tx, t0); err != nil {
			t.Fatal(err)
		}
	}
	rec("app/web", "c1", 1000, 100) // baseline
	rec("app/web", "c1", 1500, 300) // +500 +200
	rec("app/web", "c1", 50, 20)    // restarted: counters reset, count in full
	rec("app/web", "c2", 10, 5)     // re-created: new ID, count in full
	rec("app/db", "d1", 7, 7)       // baseline only
	// The same key on another host is another container.
	nas, err := s.AddHost(ctx, "nas", "agent", "docker", "test", t0)
	if err != nil {
		t.Fatal(err)
	}
	for _, rx := range []int64{100, 130} {
		if err := s.RecordUsage(ctx, id, nas, "app/web", "n1", rx, 0, t0); err != nil {
			t.Fatal(err)
		}
	}
	u, err := s.OutageUsage(ctx, id)
	if err != nil || len(u) != 3 || u[0] != (Usage{LocalHost, "local", "app/web", 560, 225}) ||
		u[1] != (Usage{nas, "nas", "app/web", 30, 0}) || u[2] != (Usage{LocalHost, "local", "app/db", 0, 0}) {
		t.Fatalf("usage = %+v, %v", u, err)
	}
	o, _, _ := s.OutageByID(ctx, id)
	if !o.Measured || o.RX != 590 || o.TX != 225 {
		t.Errorf("outage totals = %+v", o)
	}
	saveState(t, s, wan.Primary, t0.Add(time.Hour), 0)
	if u, _ := s.UsageSince(ctx, t0.Add(2*time.Hour)); len(u) != 0 {
		t.Errorf("usage after the outage = %+v", u)
	}
	if u, _ := s.UsageSince(ctx, t0); len(u) != 3 || u[0].RX != 560 || u[1].Host != "nas" {
		t.Errorf("usage since t0 = %+v", u)
	}
}

// outageEvents logs what the monitor logs for two outages: an automatic one
// and a manual one, ended by Restore now.
func outageEvents(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	for i, e := range []struct {
		typ     string
		payload map[string]any
	}{
		{"state_changed", map[string]any{"from": "primary", "to": "failover_pending", "isp": 2}},
		{"state_changed", map[string]any{"from": "failover_pending", "to": "failover", "isp": 2}},
		{"failover", map[string]any{"isp": 2}},
		{"state_changed", map[string]any{"from": "failover", "to": "failback_pending", "isp": 1}},
		{"state_changed", map[string]any{"from": "failback_pending", "to": "primary", "isp": 1}},
		{"manual_failover", map[string]any{"from": "primary", "to": "failover", "by": "alice"}},
		{"manual_restore", map[string]any{"from": "failover", "to": "primary", "by": "bob"}},
	} {
		if err := s.AddEvent(ctx, t0.Add(time.Duration(i)*time.Minute), e.typ, e.payload); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOutagesRebuiltFromEvents(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	outageEvents(t, s)
	s.Close()
	if s, err = Open(ctx, path); err != nil { // the table is empty: rebuilt on open
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Outages(ctx, OutageFilter{})
	if err != nil || len(got) != 2 {
		t.Fatalf("outages = %+v, %v", got, err)
	}
	manual, auto := got[0], got[1]
	if !auto.Started.Equal(t0.Add(time.Minute)) || !auto.Ended.Equal(t0.Add(4*time.Minute)) || auto.ISP != 2 || auto.StartedBy != "" {
		t.Errorf("automatic = %+v", auto)
	}
	if manual.StartedBy != "alice" || manual.EndedBy != "bob" || manual.ISP != 0 || manual.Duration(t0) != time.Minute {
		t.Errorf("manual = %+v", manual)
	}
}

func TestImportRebuildsOutagesFromOldBackups(t *testing.T) {
	ctx := context.Background()
	src, dst := open(t), open(t)
	fill(t, src)
	outageEvents(t, src)
	snap, err := src.Export(ctx, true, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.History["outages"]) != 0 {
		t.Fatalf("outages exported before any were recorded: %v", snap.History["outages"])
	}
	delete(snap.History, "outages") // as written by a version without them
	delete(snap.History, "outage_usage")
	if err := dst.Import(ctx, viaJSON(t, snap)); err != nil {
		t.Fatal(err)
	}
	if n := count(t, dst, "outages"); n != 2 {
		t.Errorf("outages after importing an old backup = %d, want 2", n)
	}
}
