// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"context"
	"errors"
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
