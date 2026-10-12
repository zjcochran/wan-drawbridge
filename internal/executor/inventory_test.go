// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeEvents replays events once, then ends the stream; later calls block
// until the context is done.
type fakeEvents struct {
	events []string
	calls  atomic.Int32
}

func (f *fakeEvents) Events(ctx context.Context, fn func(action, id string)) error {
	if f.calls.Add(1) == 1 {
		for _, a := range f.events {
			fn(a, "id")
		}
		return errors.New("stream closed")
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestWatchInventorySyncsOnEvents(t *testing.T) {
	f := &fakeDocker{}
	f.add("db", "app", "db", true, "")
	e, st := setup(t, f)
	var changes atomic.Int32
	e.OnChange = func() { changes.Add(1) }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	src := &fakeEvents{events: []string{"start", "create", "create"}}
	f.mu.Lock()
	f.add("web", "app", "web", true, "") // appears before the create event is handled
	f.mu.Unlock()
	go func() { e.WatchInventory(ctx, src, time.Hour); close(done) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, _ := st.Containers(context.Background())
		if len(rows) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("new container never synced: %d rows", len(rows))
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("WatchInventory did not stop with its context")
	}
	if changes.Load() == 0 {
		t.Error("OnChange was not called for the inventory change")
	}
}

func TestFailedStopIsRecorded(t *testing.T) {
	ctx := context.Background()
	f := &fakeDocker{fail: map[string]bool{"db": true}}
	f.add("db", "app", "db", true, "")
	f.add("web", "app", "web", true, "")
	e, st := setup(t, f)
	if err := st.SetStackPolicy(ctx, "app", true, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	err := e.Stop(ctx, "auto", 2)
	if err == nil || !strings.Contains(err.Error(), "stop db: refused") {
		t.Fatalf("Stop error: %v", err)
	}
	if f.running("web") || !f.running("db") {
		t.Fatalf("a failure must not stop the others being stopped: ops=%v", f.ops)
	}
	run, _ := st.OpenRun(ctx)
	if run == nil {
		t.Fatal("run should stay open after a failed stop")
	}
	actions, _ := st.RunActions(ctx, run.ID)
	var dbErr string
	for _, a := range actions {
		if a.Name == "db" {
			dbErr = a.Error
		}
	}
	if !strings.Contains(dbErr, "refused") {
		t.Errorf("the error was not recorded on the action: %+v", actions)
	}
}
