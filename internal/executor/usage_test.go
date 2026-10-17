// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/store"
	"github.com/zjcochran/wan-drawbridge/internal/wan"
)

// fakeStats returns counters by container ID; a missing ID fails, and an
// ID in none has no networks.
type fakeStats struct {
	mu       sync.Mutex
	counters map[string][2]int64
	none     map[string]bool
	calls    int
}

func (f *fakeStats) Stats(_ context.Context, id string) (int64, int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.none[id] {
		return 0, 0, false, nil
	}
	c, ok := f.counters[id]
	if !ok {
		return 0, 0, false, errors.New("no such container")
	}
	return c[0], c[1], true, nil
}

func (f *fakeStats) set(id string, rx, tx int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counters[id] = [2]int64{rx, tx}
}

func TestSampleUsage(t *testing.T) {
	ctx := context.Background()
	f := &fakeDocker{}
	f.add("web", "app", "web", true, "")
	f.add("host", "", "", true, "")         // host networking: no counters
	f.add("broken", "", "", true, "")       // stats fail
	f.add("idle", "app", "idle", false, "") // stopped: not read
	stats := &fakeStats{counters: map[string][2]int64{"id-web": {100, 10}}, none: map[string]bool{"id-host": true}}
	f.stats = stats
	e, st := setup(t, f)

	// No outage: nothing is read.
	if err := e.SampleUsage(ctx); err != nil || stats.calls != 0 {
		t.Fatalf("sample without an outage: %v, %d calls", err, stats.calls)
	}

	now := time.Unix(1_800_000_000, 0)
	if err := st.SaveState(ctx, store.WANState{Machine: wan.Machine{State: wan.Failover}, Since: now, LearningStartedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := e.SampleUsage(ctx); err != nil { // baseline
		t.Fatal(err)
	}
	stats.set("id-web", 1100, 510)
	if err := e.SampleUsage(ctx); err != nil {
		t.Fatal(err)
	}
	id, _ := st.OpenOutage(ctx)
	u, err := st.OutageUsage(ctx, id)
	if err != nil || len(u) != 1 || u[0] != (store.Usage{HostID: store.LocalHost, Host: "local", Container: "app/web", RX: 1000, TX: 500}) {
		t.Fatalf("usage = %+v, %v", u, err)
	}
}

func TestWatchUsageStops(t *testing.T) {
	f := &fakeDocker{}
	e, _ := setup(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.WatchUsage(ctx, time.Millisecond); close(done) }()
	time.Sleep(5 * time.Millisecond)
	cancel()
	<-done
}

func dataAlerts(t *testing.T, st *store.Store) []store.Event {
	t.Helper()
	evs, err := st.Events(context.Background(), store.EventFilter{Type: "data_alert"})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestDataAlerts(t *testing.T) {
	ctx := context.Background()
	f := &fakeDocker{}
	f.add("web", "app", "web", true, "")
	f.add("db", "app", "db", true, "")
	stats := &fakeStats{counters: map[string][2]int64{"id-web": {0, 0}, "id-db": {0, 0}}}
	f.stats = stats
	e, st := setup(t, f)
	now := time.Now()
	e.Now = func() time.Time { return now }
	isp, _, err := st.SeeASN(ctx, 64510, "SKYLINK-AS64510", now)
	if err != nil {
		t.Fatal(err)
	}
	st.SaveState(ctx, store.WANState{Machine: wan.Machine{State: wan.Failover}, Since: now, LearningStartedAt: now, CurrentASN: 64510})
	e.SampleUsage(ctx) // baseline, with no limits set

	if err := st.SetDataLimits(ctx, isp, store.DataLimits{OutageBytes: 1000, MonthBytes: 3000}); err != nil {
		t.Fatal(err)
	}
	stats.set("id-web", 600, 300) // 900: under both
	if err := e.SampleUsage(ctx); err != nil || len(dataAlerts(t, st)) != 0 {
		t.Fatalf("under the limits: %v, %d alerts", err, len(dataAlerts(t, st)))
	}
	stats.set("id-web", 700, 400) // 1100: over the outage limit
	e.SampleUsage(ctx)
	stats.set("id-web", 800, 400) // still over: not sent again
	e.SampleUsage(ctx)
	evs := dataAlerts(t, st)
	if len(evs) != 1 || evs[0].Payload["scope"] != "outage" || evs[0].Payload["isp"] != float64(isp) ||
		evs[0].Payload["used"] != float64(1100) || evs[0].Payload["limit"] != float64(1000) {
		t.Fatalf("outage alert = %+v", evs)
	}
	stats.set("id-db", 2000, 0) // 3200: over the month limit
	e.SampleUsage(ctx)
	e.SampleUsage(ctx)
	if evs = dataAlerts(t, st); len(evs) != 2 || evs[0].Payload["scope"] != "month" {
		t.Fatalf("month alert = %+v", evs)
	}
	var sent store.DataAlertsSent
	st.GetSetting(ctx, store.SettingDataAlertsSent, &sent)
	if id, _ := st.OpenOutage(ctx); sent.Outage != id || sent.Months[strconv.Itoa(isp)] != now.Format("2006-01") {
		t.Errorf("sent = %+v", sent)
	}
}

func TestDataAlertsWithoutISP(t *testing.T) {
	ctx := context.Background()
	f := &fakeDocker{}
	f.add("web", "app", "web", true, "")
	f.stats = &fakeStats{counters: map[string][2]int64{"id-web": {0, 0}}}
	e, st := setup(t, f)
	now := time.Now()
	st.SaveState(ctx, store.WANState{Machine: wan.Machine{State: wan.Failover}, Since: now, LearningStartedAt: now})
	if err := e.SampleUsage(ctx); err != nil || len(dataAlerts(t, st)) != 0 {
		t.Fatalf("no ISP: %v", err)
	}
}
