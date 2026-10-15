// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/agentauth"
	"github.com/zjcochran/wan-drawbridge/internal/hosts"
)

// fakeRuntime is a host with a few workloads.
type fakeRuntime struct {
	mu  sync.Mutex
	ws  []hosts.Workload
	ops []string
	err error // List fails with it
}

func newRuntime() *fakeRuntime {
	return &fakeRuntime{ws: []hosts.Workload{
		{ID: "a", Name: "media-db-1", Kind: hosts.KindContainer, Group: "media", Member: "db", Running: true},
		{ID: "b", Name: "dns", Kind: hosts.KindContainer, Running: true},
	}}
}

func (f *fakeRuntime) List(context.Context) ([]hosts.Workload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ws), f.err
}
func (f *fakeRuntime) Inspect(context.Context, hosts.Workload) (bool, string, error) {
	return true, "healthy", nil
}
func (f *fakeRuntime) Stop(_ context.Context, w hosts.Workload, d time.Duration) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "stop "+w.ID+" "+d.String())
	return []byte("r"), nil
}
func (f *fakeRuntime) Start(_ context.Context, w hosts.Workload, restore []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "start "+w.ID+" "+string(restore))
	return nil
}
func (f *fakeRuntime) Stats(_ context.Context, w hosts.Workload) (int64, int64, bool, error) {
	return 5, 6, w.ID == "a", nil
}

// fakeManager is just enough of the manager's agent API.
type fakeManager struct {
	t        *testing.T
	key      agentauth.Key
	mu       sync.Mutex
	enrolled agentauth.PublicKey
	refuse   atomic.Bool
	pending  []agentauth.Command
	results  []agentauth.Result
	inv      []agentauth.Inventory
	rotated  []string
	srv      *httptest.Server
}

func newManager(t *testing.T) *fakeManager {
	k, _ := agentauth.NewKey()
	m := &fakeManager{t: t, key: k}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+agentauth.PathEnroll, func(w http.ResponseWriter, r *http.Request) {
		var req agentauth.EnrollRequest
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck // test
		if req.Token != "dbe_good" {
			http.Error(w, "unknown or already used enrollment token", http.StatusUnauthorized)
			return
		}
		m.mu.Lock()
		m.enrolled, _ = agentauth.ParsePublicKey(req.PublicKey)
		m.mu.Unlock()
		json.NewEncoder(w).Encode(agentauth.EnrollResponse{HostID: 7, Name: "nas", ManagerKeyID: "k1", ManagerKey: k.Public().String()}) //nolint:errcheck // test
	})
	check := func(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
		body, _ := io.ReadAll(r.Body)
		if m.refuse.Load() {
			http.Error(w, "revoked", http.StatusUnauthorized)
			return nil, false
		}
		a, err := agentauth.ParseAuth(r.Header.Get("Authorization"))
		m.mu.Lock()
		keys := []agentauth.PublicKey{m.enrolled}
		m.mu.Unlock()
		if err == nil {
			_, err = a.Verify(keys, r.Method, r.URL.RequestURI(), body, time.Now())
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return nil, false
		}
		return body, true
	}
	mux.HandleFunc("GET "+agentauth.PathCommands, func(w http.ResponseWriter, r *http.Request) {
		if _, ok := check(w, r); !ok {
			return
		}
		m.mu.Lock()
		cmds := m.pending
		m.pending = nil
		m.mu.Unlock()
		if cmds == nil {
			time.Sleep(20 * time.Millisecond)
			cmds = []agentauth.Command{}
		}
		json.NewEncoder(w).Encode(agentauth.Poll{Commands: cmds}) //nolint:errcheck // test
	})
	mux.HandleFunc("POST "+agentauth.PathResults, func(w http.ResponseWriter, r *http.Request) {
		body, ok := check(w, r)
		if !ok {
			return
		}
		var rs []agentauth.Result
		json.Unmarshal(body, &rs) //nolint:errcheck // test
		m.mu.Lock()
		m.results = append(m.results, rs...)
		m.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST "+agentauth.PathInventory, func(w http.ResponseWriter, r *http.Request) {
		body, ok := check(w, r)
		if !ok {
			return
		}
		var inv agentauth.Inventory
		json.Unmarshal(body, &inv) //nolint:errcheck // test
		m.mu.Lock()
		m.inv = append(m.inv, inv)
		m.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST "+agentauth.PathRotate, func(w http.ResponseWriter, r *http.Request) {
		body, ok := check(w, r)
		if !ok {
			return
		}
		var rr agentauth.RotateRequest
		json.Unmarshal(body, &rr) //nolint:errcheck // test
		k, _ := agentauth.ParsePublicKey(rr.NewKey)
		if !k.Verify(agentauth.RotationProof(7, k, rr.TS), rr.Proof) {
			http.Error(w, "bad proof", http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		m.enrolled = k
		m.rotated = append(m.rotated, rr.NewKey)
		m.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

// command queues a signed command.
func (m *fakeManager) command(typ, workload string, args any, key agentauth.Key, host int64) agentauth.Command {
	m.t.Helper()
	c, err := agentauth.NewCommand(key, "k1", host, typ, workload, args, time.Now().Add(time.Minute))
	if err != nil {
		m.t.Fatal(err)
	}
	m.mu.Lock()
	m.pending = append(m.pending, c)
	m.mu.Unlock()
	return c
}

// invs returns the inventories sent so far.
func (m *fakeManager) invs() []agentauth.Inventory {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.inv)
}

func (m *fakeManager) rotations() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rotated)
}

func (m *fakeManager) result(id string) (agentauth.Result, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.results {
		if r.ID == id {
			return r, true
		}
	}
	return agentauth.Result{}, false
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func newAgent(t *testing.T, m *fakeManager, rt hosts.Runtime, allow ...string) (*Agent, string) {
	t.Helper()
	dir := t.TempDir()
	a := New(Config{Manager: m.srv.URL, Token: "dbe_good", ManagerKey: m.key.Public().String(), Dir: dir, Allow: allow,
		Platform: "docker", Version: "test"}, rt, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.InventoryEvery, a.RetryEvery = 20*time.Millisecond, 5*time.Millisecond
	return a, dir
}

func run(t *testing.T, a *Agent) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

func TestConfigFromEnv(t *testing.T) {
	env := map[string]string{"DRAWBRIDGE_MANAGER": "https://drawbridge.example.com/", "DRAWBRIDGE_AGENT_ALLOW": " media, ,downloads "}
	c, err := ConfigFromEnv(func(k string) string { return env[k] }, "v1.2.3")
	if err != nil || c.Manager != "https://drawbridge.example.com" || c.Dir != "/data/agent" ||
		!slices.Equal(c.Allow, []string{"media", "downloads"}) || c.Version != "v1.2.3" || c.Platform != "docker" {
		t.Fatalf("config = %+v, %v", c, err)
	}
	env["DRAWBRIDGE_AGENT_PLATFORM"] = "swarm"
	if c, err := ConfigFromEnv(func(k string) string { return env[k] }, ""); err != nil || c.Platform != PlatformSwarm {
		t.Errorf("swarm platform = %+v, %v", c, err)
	}
	env["DRAWBRIDGE_AGENT_PLATFORM"] = "lxc"
	if _, err := ConfigFromEnv(func(k string) string { return env[k] }, ""); err == nil {
		t.Error("an unknown platform was accepted")
	}
	delete(env, "DRAWBRIDGE_AGENT_PLATFORM")
	for _, bad := range []string{"", "drawbridge.example.com", "ftp://x", "https://"} {
		env["DRAWBRIDGE_MANAGER"] = bad
		if _, err := ConfigFromEnv(func(k string) string { return env[k] }, ""); err == nil {
			t.Errorf("manager %q accepted", bad)
		}
	}
}

func TestCommands(t *testing.T) {
	m := newManager(t)
	rt := newRuntime()
	a, dir := newAgent(t, m, rt)
	run(t, a)
	waitFor(t, "the inventory", func() bool { m.mu.Lock(); defer m.mu.Unlock(); return len(m.inv) > 0 })
	if inv := m.invs()[0]; len(inv.Workloads) != 2 || inv.Version != "test" || inv.Platform != "docker" {
		t.Errorf("inventory = %+v", inv)
	}
	waitFor(t, "a heartbeat", func() bool { return Healthy(dir, time.Now()) == nil })
	if err := Healthy(dir, time.Now().Add(HealthyFor+time.Second)); err == nil {
		t.Error("an old heartbeat is healthy")
	}
	if err := Healthy(t.TempDir(), time.Now()); err == nil {
		t.Error("no heartbeat is healthy")
	}

	other, _ := agentauth.NewKey()
	stop := m.command(agentauth.CmdStop, "a", agentauth.StopArgs{TimeoutSeconds: 10}, m.key, 7)
	start := m.command(agentauth.CmdStart, "a", agentauth.StartArgs{Restore: []byte("x")}, m.key, 7)
	inspect := m.command(agentauth.CmdInspect, "a", nil, m.key, 7)
	stats := m.command(agentauth.CmdStats, "", nil, m.key, 7)
	forged := m.command(agentauth.CmdStop, "a", agentauth.StopArgs{}, other, 7)
	elsewhere := m.command(agentauth.CmdStop, "a", agentauth.StopArgs{}, m.key, 8)
	unknown := m.command("reboot", "a", nil, m.key, 7)
	missing := m.command(agentauth.CmdStart, "zzz", nil, m.key, 7)
	waitFor(t, "every result", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.results) == 8
	})
	for name, c := range map[string]struct {
		cmd agentauth.Command
		ok  bool
	}{
		"stop": {stop, true}, "start": {start, true}, "inspect": {inspect, true}, "stats": {stats, true},
		"forged": {forged, false}, "for another host": {elsewhere, false}, "unknown": {unknown, false}, "missing workload": {missing, false},
	} {
		if r, _ := m.result(c.cmd.ID); r.OK != c.ok {
			t.Errorf("%s: %+v", name, r)
		}
	}
	if r, _ := m.result(stop.ID); string(r.Data) != `{"restore":"cg=="}` {
		t.Errorf("stop data = %s", r.Data)
	}
	if r, _ := m.result(stats.ID); string(r.Data) != `{"a":{"rx":5,"tx":6}}` {
		t.Errorf("stats data = %s", r.Data)
	}
	if r, _ := m.result(inspect.ID); string(r.Data) != `{"running":true,"health":"healthy"}` {
		t.Errorf("inspect data = %s", r.Data)
	}
	rt.mu.Lock()
	ops := slices.Clone(rt.ops)
	rt.mu.Unlock()
	if !slices.Contains(ops, "stop a 10s") || !slices.Contains(ops, "start a x") {
		t.Errorf("ops = %v", ops)
	}

	// The same command again is refused.
	m.mu.Lock()
	m.pending = append(m.pending, stop)
	m.results = nil
	m.mu.Unlock()
	waitFor(t, "the replay's result", func() bool { m.mu.Lock(); defer m.mu.Unlock(); return len(m.results) == 1 })
	if r, _ := m.result(stop.ID); r.OK || !strings.Contains(r.Error, "already run") {
		t.Errorf("replay = %+v", r)
	}
}

func TestAllowAndRekey(t *testing.T) {
	m := newManager(t)
	a, dir := newAgent(t, m, newRuntime(), "media")
	run(t, a)
	waitFor(t, "the inventory", func() bool { m.mu.Lock(); defer m.mu.Unlock(); return len(m.inv) > 0 })
	if ws := m.invs()[0].Workloads; len(ws) != 1 || ws[0].Group != "media" {
		t.Errorf("an agent limited to media sent %+v", ws)
	}
	outside := m.command(agentauth.CmdStop, "b", agentauth.StopArgs{}, m.key, 7)

	next, _ := agentauth.NewKey()
	rekey := m.command(agentauth.CmdRekey, "", agentauth.RekeyArgs{KeyID: "k2", Key: next.Public().String()}, m.key, 7)
	badRekey := m.command(agentauth.CmdRekey, "", agentauth.RekeyArgs{KeyID: "k3", Key: "nope"}, m.key, 7)
	waitFor(t, "results", func() bool { m.mu.Lock(); defer m.mu.Unlock(); return len(m.results) == 3 })
	if r, _ := m.result(outside.ID); r.OK || !strings.Contains(r.Error, "DRAWBRIDGE_AGENT_ALLOW") {
		t.Errorf("outside the allowlist = %+v", r)
	}
	if r, _ := m.result(rekey.ID); !r.OK || string(r.Data) != `{"key_id":"k2","key":""}` {
		t.Errorf("rekey = %+v", r)
	}
	if r, _ := m.result(badRekey.ID); r.OK {
		t.Errorf("bad rekey = %+v", r)
	}
	var st state
	b, _ := os.ReadFile(filepath.Join(dir, stateFile))
	if json.Unmarshal(b, &st) != nil || st.ManagerKeyID != "k2" || st.ManagerKey != next.Public().String() {
		t.Errorf("state = %+v", st)
	}
	// The new key works; the old one for a grace period only.
	old := m.command(agentauth.CmdInspect, "a", nil, m.key, 7)
	fresh := m.command(agentauth.CmdInspect, "a", nil, next, 7)
	waitFor(t, "results", func() bool { m.mu.Lock(); defer m.mu.Unlock(); return len(m.results) == 5 })
	if r, _ := m.result(old.ID); !r.OK {
		t.Errorf("a command signed with the old manager key within the grace period: %+v", r)
	}
	if r, _ := m.result(fresh.ID); !r.OK {
		t.Errorf("a command signed with the new manager key: %+v", r)
	}
	a.mu.Lock()
	a.st.PreviousUntil = time.Now().Add(-time.Second) // the grace period is over
	a.mu.Unlock()
	late, _ := agentauth.NewCommand(m.key, "k1", 7, agentauth.CmdInspect, "a", nil, time.Now().Add(time.Hour))
	if err := a.accept(late); err == nil {
		t.Error("the old manager key is still accepted after the grace period")
	}
}

func TestRotation(t *testing.T) {
	m := newManager(t)
	a, dir := newAgent(t, m, newRuntime())
	run(t, a)
	waitFor(t, "the inventory", func() bool { m.mu.Lock(); defer m.mu.Unlock(); return len(m.inv) > 0 })
	before, _ := os.ReadFile(filepath.Join(dir, keyFile))
	rot := m.command(agentauth.CmdRotate, "", nil, m.key, 7)
	waitFor(t, "the rotation", func() bool { r, ok := m.result(rot.ID); return ok && r.OK })
	after, _ := os.ReadFile(filepath.Join(dir, keyFile))
	if string(after) == string(before) || m.rotations() != 1 {
		t.Fatal("the key did not change")
	}
	if _, err := os.Stat(filepath.Join(dir, nextKeyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("key.next left behind")
	}
	// Requests keep working with the new key.
	n := len(m.invs())
	waitFor(t, "another inventory", func() bool { return len(m.invs()) > n })
}

// A rotation cut short (key.next saved, manager not told) is finished at the
// next start.
func TestFinishRotation(t *testing.T) {
	m := newManager(t)
	a, dir := newAgent(t, m, newRuntime())
	run(t, a)
	waitFor(t, "the inventory", func() bool { m.mu.Lock(); defer m.mu.Unlock(); return len(m.inv) > 0 })
	next, _ := agentauth.NewKey()
	if err := os.WriteFile(filepath.Join(dir, nextKeyFile), []byte(next.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	b := New(a.Cfg, newRuntime(), nil, a.Log)
	if err := b.loadKey(); err != nil {
		t.Fatal(err)
	}
	if err := b.load(); err != nil {
		t.Fatal(err)
	}
	if err := b.finishRotation(context.Background()); err != nil {
		t.Fatal(err)
	}
	k, _ := os.ReadFile(filepath.Join(dir, keyFile))
	if strings.TrimSpace(string(k)) != next.String() || m.rotations() != 1 {
		t.Error("the saved new key was not sent and used")
	}
	// A broken key.next is dropped.
	os.WriteFile(filepath.Join(dir, nextKeyFile), []byte("junk"), 0o600) //nolint:errcheck // test
	if err := b.finishRotation(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, nextKeyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("a broken key.next was kept")
	}
}

func TestEnrollFailures(t *testing.T) {
	m := newManager(t)
	other, _ := agentauth.NewKey()
	for name, c := range map[string]struct {
		token, key, want string
	}{
		"no token":      {"", m.key.Public().String(), "DRAWBRIDGE_ENROLL_TOKEN"},
		"bad pin":       {"dbe_good", "nope", "DRAWBRIDGE_MANAGER_KEY"},
		"wrong token":   {"dbe_bad", m.key.Public().String(), "already used"},
		"wrong manager": {"dbe_good", other.Public().String(), "not the one in DRAWBRIDGE_MANAGER_KEY"},
	} {
		a, _ := newAgent(t, m, newRuntime())
		a.Cfg.Token, a.Cfg.ManagerKey = c.token, c.key
		err := a.Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	a, _ := newAgent(t, m, newRuntime())
	a.HTTP = &http.Client{Transport: failing{}}
	if err := a.Run(context.Background()); err == nil {
		t.Error("enrolled without reaching the manager")
	}
}

type failing struct{}

func (failing) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("unreachable")
}

// When the manager refuses the agent or cannot be reached, it keeps trying.
func TestPollFailures(t *testing.T) {
	m := newManager(t)
	a, _ := newAgent(t, m, newRuntime())
	run(t, a)
	waitFor(t, "the inventory", func() bool { m.mu.Lock(); defer m.mu.Unlock(); return len(m.inv) > 0 })
	m.refuse.Store(true)
	time.Sleep(50 * time.Millisecond)
	m.refuse.Store(false)

	// An enrolled agent that cannot reach the manager keeps trying.
	b, dir := newAgent(t, m, newRuntime())
	st, _ := json.Marshal(state{HostID: 7, Name: "nas", ManagerKeyID: "k1", ManagerKey: m.key.Public().String(), KeyCreated: time.Now()})
	if err := os.WriteFile(filepath.Join(dir, stateFile), st, 0o600); err != nil {
		t.Fatal(err)
	}
	b.HTTP = &http.Client{Transport: failing{}}
	run(t, b)
	time.Sleep(30 * time.Millisecond)
	if err := Healthy(dir, time.Now()); err == nil {
		t.Error("an agent that never reached the manager is healthy")
	}
}

func TestInventoryEvents(t *testing.T) {
	m := newManager(t)
	rt := newRuntime()
	a, _ := newAgent(t, m, rt)
	a.InventoryEvery = time.Hour
	ev := &events{}
	a.Events = ev
	run(t, a)
	waitFor(t, "the first inventory", func() bool { m.mu.Lock(); defer m.mu.Unlock(); return len(m.inv) == 1 })
	waitFor(t, "an inventory after the events", func() bool { m.mu.Lock(); defer m.mu.Unlock(); return len(m.inv) >= 2 })
	rt.mu.Lock()
	rt.err = errors.New("docker is down")
	rt.mu.Unlock()
	if err := a.pushInventory(context.Background()); err == nil {
		t.Error("an inventory was sent without a listing")
	}
}

// events reports one create, then ends the stream; later calls wait.
type events struct{ calls atomic.Int32 }

func (e *events) Events(ctx context.Context, fn func(action, id string)) error {
	if e.calls.Add(1) == 1 {
		fn("start", "x")
		return errors.New("stream closed")
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestPlainHTTPAndBadState(t *testing.T) {
	m := newManager(t)
	a, dir := newAgent(t, m, newRuntime())
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(context.Background()); err == nil {
		t.Error("ran with a broken state file")
	}
	os.WriteFile(filepath.Join(dir, keyFile), []byte("junk"), 0o600) //nolint:errcheck // test
	b, _ := newAgent(t, m, newRuntime())
	b.Cfg.Dir = dir
	if err := b.Run(context.Background()); err == nil {
		t.Error("ran with a broken key file")
	}
}
