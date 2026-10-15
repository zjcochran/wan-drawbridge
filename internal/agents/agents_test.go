// SPDX-License-Identifier: AGPL-3.0-or-later

package agents_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/agent"
	"github.com/zjcochran/wan-drawbridge/internal/agentauth"
	"github.com/zjcochran/wan-drawbridge/internal/agents"
	"github.com/zjcochran/wan-drawbridge/internal/docker"
	"github.com/zjcochran/wan-drawbridge/internal/executor"
	"github.com/zjcochran/wan-drawbridge/internal/hosts"
	"github.com/zjcochran/wan-drawbridge/internal/secrets"
	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// fakeDocker is a Docker engine with a few containers.
type fakeDocker struct {
	mu  sync.Mutex
	cs  []docker.Container
	ops []string
}

func newDocker() *fakeDocker {
	return &fakeDocker{cs: []docker.Container{
		{ID: "id-db", Name: "media-db-1", Project: "media", Service: "db", Running: true},
		{ID: "id-player", Name: "media-player-1", Project: "media", Service: "player", Running: true,
			Labels: map[string]string{docker.LabelDependsOn: "db:service_started:false"}},
		{ID: "id-dns", Name: "dns", Running: true},
	}}
}

func (f *fakeDocker) List(context.Context) ([]docker.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.cs), nil
}

func (f *fakeDocker) set(id string, running bool, op string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.cs {
		if f.cs[i].ID == id {
			f.cs[i].Running = running
			f.ops = append(f.ops, op+" "+f.cs[i].Name)
			return nil
		}
	}
	return errors.New("no such container")
}

func (f *fakeDocker) Inspect(_ context.Context, id string) (bool, docker.Health, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cs {
		if c.ID == id {
			return c.Running, "healthy", nil
		}
	}
	return false, "", errors.New("no such container")
}

func (f *fakeDocker) Stop(_ context.Context, id string, _ time.Duration) error {
	return f.set(id, false, "stop")
}
func (f *fakeDocker) Start(_ context.Context, id string) error { return f.set(id, true, "start") }
func (f *fakeDocker) Stats(_ context.Context, id string) (int64, int64, bool, error) {
	return 1000, 10, id != "id-dns", nil
}

func (f *fakeDocker) running(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cs {
		if c.Name == name {
			return c.Running
		}
	}
	return false
}

type env struct {
	t     *testing.T
	ctx   context.Context
	st    *store.Store
	mgr   *agents.Manager
	reg   *hosts.Registry
	url   string
	local *fakeDocker
	log   *slog.Logger
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, _ := secrets.New("test-key")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	local := newDocker()
	lh := hosts.Host{ID: store.LocalHost, Name: "local", Runtime: hosts.Local{Docker: local}}
	reg := hosts.NewRegistry(lh)
	mgr := agents.New(st, box, reg, lh, log)
	if err := mgr.Load(ctx); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mgr.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, ctx: ctx, st: st, mgr: mgr, reg: reg, url: srv.URL, local: local, log: log}
}

// startAgent adds the host "nas" and runs an agent for it until the test
// ends.
func (e *env) startAgent(d *fakeDocker, allow ...string) (int64, string) {
	e.t.Helper()
	id, token, err := e.mgr.Add(e.ctx, "nas", "alice")
	if err != nil {
		e.t.Fatal(err)
	}
	key, err := e.mgr.ManagerKey(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	dir := e.t.TempDir()
	a := agent.New(agent.Config{Manager: e.url, Token: token, ManagerKey: key.String(), Dir: dir, Allow: allow,
		Platform: "docker", Version: "test"}, hosts.Local{Docker: d}, nil, e.log)
	a.InventoryEvery = 50 * time.Millisecond
	a.RetryEvery = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }() //nolint:errcheck // checked through the manager
	e.t.Cleanup(func() { cancel(); <-done })
	e.waitFor("the agent's inventory", func() bool {
		h, ok := e.reg.Get(id)
		if !ok {
			return false
		}
		ws, err := h.Runtime.List(e.ctx)
		return err == nil && len(ws) > 0
	})
	return id, dir
}

// signed makes a GET request signed with key and returns its status.
func (e *env) signed(key agentauth.Key, host int64, path string) int {
	e.t.Helper()
	auth, err := agentauth.SignRequest(key, host, http.MethodGet, path, nil, time.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(e.ctx, http.MethodGet, e.url+path, nil)
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (e *env) waitFor(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *env) agentRow(id int64) store.AgentHost {
	e.t.Helper()
	a, ok, err := e.st.Agent(e.ctx, id)
	if err != nil || !ok {
		e.t.Fatalf("agent %d: %v", id, err)
	}
	return a
}

func (e *env) events(typ string) []store.Event {
	evs, _ := e.st.Events(e.ctx, store.EventFilter{Type: typ, Limit: 10})
	return evs
}

func TestFailoverThroughAnAgent(t *testing.T) {
	e := newEnv(t)
	nasDocker := newDocker()
	id, _ := e.startAgent(nasDocker)
	row := e.agentRow(id)
	if row.Status != store.HostOnline || row.Version != "test" || row.Platform != "docker" || row.PublicKey == "" || row.ManagerKeyID != "k1" {
		t.Fatalf("enrolled host = %+v", row)
	}
	if len(e.events("host_enrolled")) != 1 || len(e.events("host_added")) != 1 {
		t.Error("enrollment events missing")
	}

	ex := executor.New(e.reg, e.st, e.log)
	ex.PollEvery = 10 * time.Millisecond
	if err := ex.Sync(e.ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetStackPolicy(e.ctx, id, "media", true, "alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := ex.Stop(e.ctx, "auto", 0); err != nil {
		t.Fatal(err)
	}
	if nasDocker.running("media-db-1") || nasDocker.running("media-player-1") || !nasDocker.running("dns") || !e.local.running("media-db-1") {
		t.Fatalf("after the failover: nas %v, local %v", nasDocker.ops, e.local.ops)
	}
	if err := ex.Start(e.ctx); err != nil {
		t.Fatal(err)
	}
	if !nasDocker.running("media-db-1") || !nasDocker.running("media-player-1") {
		t.Fatalf("after the failback: %v", nasDocker.ops)
	}
	if i, j := slices.Index(nasDocker.ops, "start media-db-1"), slices.Index(nasDocker.ops, "start media-player-1"); i > j {
		t.Errorf("dependencies must start first: %v", nasDocker.ops)
	}
}

func TestAgentRuntime(t *testing.T) {
	e := newEnv(t)
	id, _ := e.startAgent(newDocker())
	h, _ := e.reg.Get(id)
	if rx, tx, ok, err := h.Runtime.Stats(e.ctx, hosts.Workload{ID: "id-db"}); err != nil || !ok || rx != 1000 || tx != 10 {
		t.Errorf("stats = %d %d %v %v", rx, tx, ok, err)
	}
	if _, _, ok, err := h.Runtime.Stats(e.ctx, hosts.Workload{ID: "id-dns"}); err != nil || ok {
		t.Errorf("stats of a container without counters = %v %v", ok, err)
	}
	if running, health, err := h.Runtime.Inspect(e.ctx, hosts.Workload{ID: "id-db"}); err != nil || !running || health != "healthy" {
		t.Errorf("inspect = %v %q %v", running, health, err)
	}
	if err := h.Runtime.Start(e.ctx, hosts.Workload{ID: "id-gone"}, nil); err == nil {
		t.Error("starting an unknown workload worked")
	}
}

func TestAllowList(t *testing.T) {
	e := newEnv(t)
	nasDocker := newDocker()
	id, _ := e.startAgent(nasDocker, "media")
	h, _ := e.reg.Get(id)
	ws, _ := h.Runtime.List(e.ctx)
	if len(ws) != 2 {
		t.Fatalf("an agent limited to media lists %+v", ws)
	}
	err := h.Runtime.Start(e.ctx, hosts.Workload{ID: "id-dns"}, nil)
	if err == nil || !nasDocker.running("dns") {
		t.Fatalf("a container outside the allowlist was acted on: %v", err)
	}
}

func TestKeyRotation(t *testing.T) {
	e := newEnv(t)
	id, _ := e.startAgent(newDocker())
	before := e.agentRow(id).PublicKey
	if err := e.mgr.RotateNow(e.ctx, id); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the new key in use", func() bool {
		a := e.agentRow(id)
		return a.PublicKey != before && a.PreviousKey == ""
	})
	if evs := e.events("host_key_rotated"); len(evs) != 1 || evs[0].Payload["host"] != "nas" {
		t.Errorf("events = %+v", evs)
	}
	// Commands still work with the new key.
	h, _ := e.reg.Get(id)
	if _, _, err := h.Runtime.Inspect(e.ctx, hosts.Workload{ID: "id-db"}); err != nil {
		t.Fatal(err)
	}
}

func TestManagerKeyRotation(t *testing.T) {
	e := newEnv(t)
	id, _ := e.startAgent(newDocker())
	old, _ := e.mgr.ManagerKey(e.ctx)
	if err := e.mgr.RotateManagerKey(e.ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	now, _ := e.mgr.ManagerKey(e.ctx)
	if now.Equal(old) {
		t.Fatal("the manager key did not change")
	}
	e.waitFor("the agent to move", func() bool {
		moved, total, err := e.mgr.KeyProgress(e.ctx)
		return err == nil && moved == 1 && total == 1
	})
	if e.agentRow(id).ManagerKeyID != "k2" {
		t.Errorf("pinned key = %s", e.agentRow(id).ManagerKeyID)
	}
	// Commands are now signed with the new key, which the agent accepts.
	h, _ := e.reg.Get(id)
	if _, _, err := h.Runtime.Inspect(e.ctx, hosts.Workload{ID: "id-db"}); err != nil {
		t.Fatal(err)
	}
	if len(e.events("manager_key_rotated")) != 1 {
		t.Error("manager_key_rotated missing")
	}
}

func TestRevokeAndNewToken(t *testing.T) {
	e := newEnv(t)
	id, dir := e.startAgent(newDocker())
	if err := e.mgr.Revoke(e.ctx, id, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.reg.Get(id); ok {
		t.Fatal("a revoked host is still in the registry")
	}
	if a := e.agentRow(id); a.Status != store.HostRevoked || a.PublicKey != "" {
		t.Fatalf("revoked = %+v", a)
	}
	// The agent's key no longer opens anything.
	b, _ := os.ReadFile(filepath.Join(dir, "key"))
	key, err := agentauth.ParseKey(string(b))
	if err != nil {
		t.Fatal(err)
	}
	if code := e.signed(key, id, agentauth.PathCommands+"?wait=0"); code != http.StatusUnauthorized {
		t.Errorf("a revoked agent's request: %d", code)
	}
	token, err := e.mgr.NewToken(e.ctx, id, "alice")
	if err != nil || token == "" {
		t.Fatalf("new token: %v", err)
	}
	if a := e.agentRow(id); a.Status != store.HostPending || !a.EnrollPending {
		t.Errorf("after a new token = %+v", a)
	}
	if err := e.mgr.Revoke(e.ctx, 99, "alice"); err == nil {
		t.Error("revoked an unknown host")
	}
}

func TestOfflineAndBack(t *testing.T) {
	e := newEnv(t)
	clock := time.Now()
	var mu sync.Mutex
	e.mgr.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	// Enroll by hand: no agent polls in the background.
	_, token, err := e.mgr.Add(e.ctx, "nas", "alice")
	if err != nil {
		t.Fatal(err)
	}
	key, _ := agentauth.NewKey()
	if code, body := e.post(nil, 0, agentauth.PathEnroll, agentauth.EnrollRequest{Token: token, PublicKey: key.Public().String()}); code != http.StatusOK {
		t.Fatalf("enroll: %d %s", code, body)
	}
	id := e.agentRow(2).ID

	mu.Lock()
	clock = clock.Add(agents.OfflineAfter + time.Second)
	mu.Unlock()
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	go e.mgr.Watch(ctx, 5*time.Millisecond)
	e.waitFor("host_offline", func() bool { return len(e.events("host_offline")) == 1 })
	if a := e.agentRow(id); a.Status != store.HostOffline || a.LastError == "" {
		t.Errorf("offline host = %+v", a)
	}
	h, _ := e.reg.Get(id)
	if _, err := h.Runtime.List(e.ctx); !errors.Is(err, agents.ErrOffline) {
		t.Errorf("listing an offline host: %v", err)
	}
	if err := h.Runtime.Start(e.ctx, hosts.Workload{ID: "x"}, nil); !errors.Is(err, agents.ErrOffline) {
		t.Errorf("a command to an offline host: %v", err)
	}

	// Its next request brings it back.
	mu.Lock()
	clock = time.Now()
	mu.Unlock()
	if code, _ := e.post(&key, id, agentauth.PathInventory, agentauth.Inventory{}); code != http.StatusNoContent {
		t.Fatalf("inventory: %d", code)
	}
	if len(e.events("host_online")) != 1 || e.agentRow(id).Status != store.HostOnline {
		t.Error("the host did not come back online")
	}
}
