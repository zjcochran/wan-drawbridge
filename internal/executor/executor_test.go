// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/docker"
	"github.com/zjcochran/wan-drawbridge/internal/hosts"
	"github.com/zjcochran/wan-drawbridge/internal/store"
)

type fakeContainer struct {
	docker.Container
	health []docker.Health // returned in turn by Inspect; the last one repeats
}

type fakeDocker struct {
	mu    sync.Mutex
	cs    []*fakeContainer
	ops   []string        // "stop name" / "start name", in call order
	fail  map[string]bool // container names whose stop and start fail
	down  error           // set: the host cannot be reached
	stats *fakeStats      // network counters; nil: none
}

func (f *fakeDocker) add(name, project, service string, running bool, deps string, health ...docker.Health) {
	labels := map[string]string{}
	if project != "" {
		labels[docker.LabelProject], labels[docker.LabelService] = project, service
	}
	if deps != "" {
		labels[docker.LabelDependsOn] = deps
	}
	f.cs = append(f.cs, &fakeContainer{docker.Container{
		ID: "id-" + name, Name: name, Project: project, Service: service, Running: running, Labels: labels,
	}, health})
}

func (f *fakeDocker) find(id string) *fakeContainer {
	for _, c := range f.cs {
		if c.ID == id {
			return c
		}
	}
	return nil
}

func (f *fakeDocker) List(context.Context) ([]docker.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down != nil {
		return nil, f.down
	}
	var out []docker.Container
	for _, c := range f.cs {
		out = append(out, c.Container)
	}
	return out, nil
}

func (f *fakeDocker) Inspect(_ context.Context, id string) (bool, docker.Health, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.find(id)
	if c == nil {
		return false, "", fmt.Errorf("no such container")
	}
	var h docker.Health
	if len(c.health) > 0 {
		h = c.health[0]
		if len(c.health) > 1 {
			c.health = c.health[1:]
		}
	}
	return c.Running, h, nil
}

func (f *fakeDocker) set(id string, running bool, op string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.find(id)
	if c == nil {
		return fmt.Errorf("no such container")
	}
	if f.fail[c.Name] {
		return fmt.Errorf("%s %s: refused", op, c.Name)
	}
	c.Running = running
	f.ops = append(f.ops, op+" "+c.Name)
	return nil
}

func (f *fakeDocker) Stop(_ context.Context, id string, _ time.Duration) error {
	return f.set(id, false, "stop")
}

func (f *fakeDocker) Start(_ context.Context, id string) error { return f.set(id, true, "start") }

func (f *fakeDocker) Stats(ctx context.Context, id string) (int64, int64, bool, error) {
	if f.stats == nil {
		return 0, 0, false, nil
	}
	return f.stats.Stats(ctx, id)
}

func (f *fakeDocker) setDown(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = err
}

func (f *fakeDocker) running(name string) bool {
	for _, c := range f.cs {
		if c.Name == name {
			return c.Running
		}
	}
	return false
}

func setup(t *testing.T, f *fakeDocker) (*Executor, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	reg := hosts.NewRegistry(hosts.Host{ID: store.LocalHost, Name: "local", Runtime: hosts.Local{Docker: f}})
	e := New(reg, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.Parallel, e.PollEvery = 1, time.Millisecond // serial, so ops order is deterministic
	if err := e.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	return e, st
}

func index(ops []string, op string) int {
	i := slices.Index(ops, op)
	if i < 0 {
		panic("missing op " + op)
	}
	return i
}

func TestFailoverAndFailback(t *testing.T) {
	ctx := context.Background()
	f := &fakeDocker{}
	f.add("db", "app", "db", true, "")
	f.add("web", "app", "web", true, "db:service_started:false")
	f.add("worker", "app", "worker", false, "db:service_started:false") // already stopped
	f.add("dns", "", "", true, "")                                      // standalone, kept
	e, st := setup(t, f)
	if err := st.SetStackPolicy(ctx, store.LocalHost, "app", true, "test", time.Now()); err != nil {
		t.Fatal(err)
	}

	if err := e.Stop(ctx, "auto", 64510); err != nil {
		t.Fatal(err)
	}
	if f.running("db") || f.running("web") || !f.running("dns") {
		t.Fatalf("after stop: ops=%v", f.ops)
	}
	if index(f.ops, "stop web") > index(f.ops, "stop db") {
		t.Errorf("dependents must stop first: %v", f.ops)
	}
	if slices.Contains(f.ops, "stop worker") {
		t.Errorf("already-stopped container was stopped again: %v", f.ops)
	}

	f.ops = nil
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.running("db") || !f.running("web") || f.running("worker") {
		t.Fatalf("after start: ops=%v", f.ops)
	}
	if index(f.ops, "start db") > index(f.ops, "start web") {
		t.Errorf("dependencies must start first: %v", f.ops)
	}
	if run, _ := st.OpenRun(ctx); run != nil {
		t.Errorf("run left open: %+v", run)
	}
}

func TestRepeatedStopKeepsWasRunning(t *testing.T) {
	ctx := context.Background()
	f := &fakeDocker{}
	f.add("db", "app", "db", true, "")
	e, st := setup(t, f)
	st.SetStackPolicy(ctx, store.LocalHost, "app", true, "test", time.Now())

	// A crash between stopping and saving state means Stop runs twice.
	e.Stop(ctx, "auto", 0)
	e.Stop(ctx, "auto", 0)
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.running("db") {
		t.Fatal("db must be restarted: the second stop pass saw it stopped but must not overwrite was_running")
	}
}

func TestStartWithoutRunDoesNothing(t *testing.T) {
	f := &fakeDocker{}
	f.add("db", "app", "db", false, "")
	e, _ := setup(t, f)
	if err := e.Start(context.Background()); err != nil || len(f.ops) != 0 {
		t.Fatalf("err=%v ops=%v", err, f.ops)
	}
}

func TestSelfIsNeverStopped(t *testing.T) {
	ctx := context.Background()
	f := &fakeDocker{}
	f.add("drawbridge", "infra", "drawbridge", true, "")
	f.add("proxy", "infra", "proxy", true, "")
	f.cs[0].Labels[docker.LabelSelf] = "true"
	e, st := setup(t, f)
	st.SetStackPolicy(ctx, store.LocalHost, "infra", true, "test", time.Now())

	e.Stop(ctx, "auto", 0)
	if !f.running("drawbridge") || f.running("proxy") {
		t.Fatalf("ops=%v", f.ops)
	}
	cs, _ := st.Containers(ctx)
	for _, c := range cs {
		if c.Key == "infra/drawbridge" {
			t.Error("Drawbridge itself must not appear in the inventory")
		}
	}
}

func TestRecreatedContainerFoundByKey(t *testing.T) {
	ctx := context.Background()
	f := &fakeDocker{}
	f.add("web", "app", "web", true, "")
	e, st := setup(t, f)
	st.SetStackPolicy(ctx, store.LocalHost, "app", true, "test", time.Now())
	e.Stop(ctx, "auto", 0)

	f.cs[0].ID = "id-web-recreated" // compose re-created it while on backup
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.running("web") {
		t.Fatalf("re-created container not started: ops=%v", f.ops)
	}
}

func TestWaitsForHealthBeforeDependents(t *testing.T) {
	ctx := context.Background()
	f := &fakeDocker{}
	f.add("db", "app", "db", true, "", "starting", "starting", "healthy")
	f.add("web", "app", "web", true, "db:service_healthy:false")
	e, st := setup(t, f)
	st.SetStackPolicy(ctx, store.LocalHost, "app", true, "test", time.Now())
	e.Stop(ctx, "auto", 0)

	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.cs[0].health) != 1 {
		t.Errorf("expected health to be polled until healthy, left %v", f.cs[0].health)
	}
}

func TestLayersIgnoreCycles(t *testing.T) {
	js := []job{
		{w: hosts.Workload{Name: "a", Group: "p", Member: "a", DependsOn: []string{"b"}}},
		{w: hosts.Workload{Name: "b", Group: "p", Member: "b", DependsOn: []string{"a"}}},
		{w: hosts.Workload{Name: "c", Group: "q", Member: "c"}},
	}
	n := 0
	for _, l := range layers(js) {
		n += len(l)
	}
	if n != 3 {
		t.Fatalf("every container must be placed exactly once, got %v", layers(js))
	}
}

// Dependencies never link containers on different hosts.
func TestLayersPerHost(t *testing.T) {
	js := []job{
		{w: hosts.Workload{Host: 1, Name: "web", Group: "app", Member: "web", DependsOn: []string{"db"}}},
		{w: hosts.Workload{Host: 2, Name: "db", Group: "app", Member: "db"}},
	}
	if ls := layers(js); len(ls) != 1 {
		t.Fatalf("layers = %v", ls)
	}
}

func TestApplyDuringFailover(t *testing.T) {
	ctx := context.Background()
	f := &fakeDocker{}
	f.add("db", "app", "db", true, "")
	f.add("web", "app", "web", true, "")
	f.add("dns", "", "", true, "")
	e, st := setup(t, f)
	now := time.Now()
	st.SetStackPolicy(ctx, store.LocalHost, "app", true, "test", now)
	e.Stop(ctx, "auto", 0)

	// During failover: keep web running, and disable dns too.
	st.SetContainerPolicy(ctx, store.LocalHost, "app/web", store.Keep, "test", now)
	st.SetContainerPolicy(ctx, store.LocalHost, "dns", store.Disable, "test", now)
	if err := e.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.running("web") || f.running("dns") || f.running("db") {
		t.Fatalf("after apply: ops=%v", f.ops)
	}

	// Disable web again: stopped once more, and restarted at failback.
	st.SetContainerPolicy(ctx, store.LocalHost, "app/web", store.Disable, "test", now)
	e.Apply(ctx)
	if f.running("web") {
		t.Fatal("web should be stopped again")
	}
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.running("web") || !f.running("db") || !f.running("dns") {
		t.Fatalf("after failback: ops=%v", f.ops)
	}
}

func TestLabelsDecideWhatStops(t *testing.T) {
	ctx := context.Background()
	f := &fakeDocker{}
	f.add("db", "app", "db", true, "")
	f.add("web", "app", "web", true, "db:service_started:false")
	f.add("cache", "", "", true, "")
	f.cs[1].Labels[docker.LabelOnFailover] = "keep" // web stays although the stack stops
	f.cs[2].Labels[docker.LabelOnFailover] = "stop" // a standalone container that stops
	e, st := setup(t, f)
	if err := st.SetStackPolicy(ctx, store.LocalHost, "app", true, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(ctx, "auto", 64510); err != nil {
		t.Fatal(err)
	}
	if f.running("db") || !f.running("web") || f.running("cache") {
		t.Fatalf("ops = %v", f.ops)
	}
}
