// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/hosts"
	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// setupTwo is setup with a second host, "nas", whose stack "app" also stops.
func setupTwo(t *testing.T, local, nas *fakeDocker) (*Executor, *store.Store, int64) {
	t.Helper()
	e, st := setup(t, local)
	ctx := context.Background()
	id, err := st.AddHost(ctx, "nas", "agent", "docker", "test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e.Hosts.Set(hosts.Host{ID: id, Name: "nas", Runtime: hosts.Local{Docker: nas}})
	if err := e.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	for _, h := range []int64{store.LocalHost, id} {
		if err := st.SetStackPolicy(ctx, h, "app", true, "test", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	return e, st, id
}

func twoHosts() (local, nas *fakeDocker) {
	local, nas = &fakeDocker{}, &fakeDocker{}
	local.add("web", "app", "web", true, "")
	nas.add("web", "app", "web", true, "") // the same project and container ID on another host
	nas.add("db", "app", "db", true, "")
	return local, nas
}

func TestStopsAndStartsOnEveryHost(t *testing.T) {
	ctx := context.Background()
	local, nas := twoHosts()
	e, st, id := setupTwo(t, local, nas)
	if err := e.Stop(ctx, "auto", 0); err != nil {
		t.Fatal(err)
	}
	if local.running("web") || nas.running("web") || nas.running("db") {
		t.Fatalf("after stop: local %v, nas %v", local.ops, nas.ops)
	}
	run, _ := st.OpenRun(ctx)
	as, _ := st.RunActions(ctx, run.ID)
	if len(as) != 3 || as[0].HostID != store.LocalHost || as[1].HostID != id || as[1].Host != "nas" {
		t.Fatalf("actions = %+v", as)
	}
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !local.running("web") || !nas.running("web") || !nas.running("db") {
		t.Fatalf("after start: local %v, nas %v", local.ops, nas.ops)
	}
}

func TestStopCatchesUpWithAHostThatWasDown(t *testing.T) {
	ctx := context.Background()
	local, nas := twoHosts()
	e, st, _ := setupTwo(t, local, nas)
	nas.setDown(errors.New("connection refused"))

	err := e.Stop(ctx, "auto", 0)
	if err == nil || !strings.Contains(err.Error(), "cannot reach host nas") || !strings.Contains(err.Error(), "stopped when it is back") {
		t.Fatalf("Stop = %v", err)
	}
	if local.running("web") || !nas.running("web") {
		t.Fatal("the reachable host must be stopped and the other left as it was")
	}
	hs, _ := st.Hosts(ctx)
	if hs[1].Status != store.HostOffline || hs[1].LastError != "connection refused" {
		t.Errorf("host status = %+v", hs[1])
	}
	if rows, _ := st.Containers(ctx); len(rows) != 3 {
		t.Errorf("the unreachable host's containers must be kept: %+v", rows)
	}

	// Still down: Retry tries again and leaves things as they are.
	if err := e.Retry(ctx); err == nil {
		t.Error("Retry while the host is still down should report it")
	}
	nas.setDown(nil)
	if err := e.Retry(ctx); err != nil {
		t.Fatal(err)
	}
	if nas.running("web") || nas.running("db") {
		t.Fatalf("Retry did not stop the host that came back: %v", nas.ops)
	}
	// Nothing missed any more: Retry has nothing to do.
	nas.ops = nil
	if err := e.Retry(ctx); err != nil || len(nas.ops) != 0 {
		t.Errorf("Retry with nothing owed: %v, ops %v", err, nas.ops)
	}
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !nas.running("web") || !local.running("web") {
		t.Fatal("not everything was started at the failback")
	}
}

func TestStartOwedToAHostThatIsDown(t *testing.T) {
	ctx := context.Background()
	local, nas := twoHosts()
	e, st, id := setupTwo(t, local, nas)
	if err := e.Stop(ctx, "auto", 0); err != nil {
		t.Fatal(err)
	}
	nas.setDown(errors.New("timeout"))
	err := e.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "started when it is back") {
		t.Fatalf("Start = %v", err)
	}
	if !local.running("web") {
		t.Fatal("the reachable host was not started")
	}
	if run, _ := st.OpenRun(ctx); run != nil {
		t.Fatal("the run must end at the failback even with a host down")
	}
	pend, _ := st.PendingStarts(ctx)
	if len(pend) != 2 || pend[0].HostID != id {
		t.Fatalf("pending = %+v", pend)
	}

	if err := e.Retry(ctx); err != nil {
		t.Fatalf("Retry with the host still down: %v", err)
	}
	nas.setDown(nil)
	if err := e.Retry(ctx); err != nil {
		t.Fatal(err)
	}
	if !nas.running("web") || !nas.running("db") {
		t.Fatalf("owed starts not done: %v", nas.ops)
	}
	if pend, _ := st.PendingStarts(ctx); len(pend) != 0 {
		t.Errorf("still pending: %+v", pend)
	}
}

// A failover that begins before the host is back takes over what is owed, so
// nothing is started in the middle of the new failover.
func TestNewFailoverTakesOverOwedStarts(t *testing.T) {
	ctx := context.Background()
	local, nas := twoHosts()
	e, st, _ := setupTwo(t, local, nas)
	e.Stop(ctx, "auto", 0)
	nas.setDown(errors.New("timeout"))
	e.Start(ctx)

	nas.setDown(nil)
	if err := e.Stop(ctx, "auto", 0); err != nil {
		t.Fatal(err)
	}
	if pend, _ := st.PendingStarts(ctx); len(pend) != 0 {
		t.Fatalf("the new run did not take over: %+v", pend)
	}
	if nas.running("web") {
		t.Fatal("started during the new failover")
	}
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !nas.running("web") || !nas.running("db") {
		t.Fatalf("not started at the new failback: %v", nas.ops)
	}
}

// A host that is no longer connected (removed from the registry) keeps its
// starts owed.
func TestStartOwedToAHostThatIsGone(t *testing.T) {
	ctx := context.Background()
	local, nas := twoHosts()
	e, st, id := setupTwo(t, local, nas)
	e.Stop(ctx, "auto", 0)
	e.Hosts.Remove(id)
	err := e.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "is not connected") {
		t.Fatalf("Start = %v", err)
	}
	if pend, _ := st.PendingStarts(ctx); len(pend) != 2 {
		t.Fatalf("pending = %+v", pend)
	}
}

func TestSyncReportsUnreachableHosts(t *testing.T) {
	ctx := context.Background()
	local, nas := twoHosts()
	e, st, _ := setupTwo(t, local, nas)
	nas.setDown(errors.New("no route to host"))
	if err := e.Sync(ctx); err == nil || !strings.Contains(err.Error(), "kept as they were") {
		t.Fatalf("Sync = %v", err)
	}
	nas.setDown(nil)
	if err := e.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if hs, _ := st.Hosts(ctx); hs[1].Status != store.HostOnline {
		t.Errorf("status after it came back = %s", hs[1].Status)
	}
}

func TestApplySkipsAHostThatIsGone(t *testing.T) {
	ctx := context.Background()
	local, nas := twoHosts()
	e, st, id := setupTwo(t, local, nas)
	e.Stop(ctx, "auto", 0)
	e.Hosts.Remove(id)
	if _, err := st.SetContainerPolicy(ctx, id, "app/web", store.Keep, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	e.Apply(ctx)
	if slices.Contains(nas.ops, "start web") {
		t.Fatal("started on a host that is not connected")
	}
}
