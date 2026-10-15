// SPDX-License-Identifier: AGPL-3.0-or-later

package agents_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/agentauth"
	"github.com/zjcochran/wan-drawbridge/internal/agents"
	"github.com/zjcochran/wan-drawbridge/internal/hosts"
	"github.com/zjcochran/wan-drawbridge/internal/store"
)

func TestHostAdmin(t *testing.T) {
	e := newEnv(t)
	id, _ := e.startAgent(newDocker())
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	must(e.mgr.Rename(e.ctx, id, "attic", "alice"))
	if h, _ := e.reg.Get(id); h.Name != "attic" {
		t.Errorf("registry name = %q", h.Name)
	}
	if err := e.mgr.Rename(e.ctx, id, "local", "alice"); err == nil {
		t.Error("renamed to an existing name")
	}
	if err := e.mgr.Rename(e.ctx, 99, "x", "alice"); err == nil {
		t.Error("renamed an unknown host")
	}
	must(e.mgr.Rename(e.ctx, store.LocalHost, "here", "alice"))
	if e.mgr.Local.Name != "here" {
		t.Error("the local host keeps its old name for re-enabling")
	}

	// Disabling takes a host out of the registry; enabling puts it back.
	must(e.mgr.SetEnabled(e.ctx, id, false, "alice"))
	if _, ok := e.reg.Get(id); ok {
		t.Error("a disabled agent host is still in the registry")
	}
	must(e.mgr.SetEnabled(e.ctx, id, true, "alice"))
	if _, ok := e.reg.Get(id); !ok {
		t.Error("an enabled agent host is not back in the registry")
	}
	must(e.mgr.SetEnabled(e.ctx, store.LocalHost, false, "alice"))
	if _, ok := e.reg.Get(store.LocalHost); ok {
		t.Error("the disabled local host is still in the registry")
	}
	must(e.mgr.SetEnabled(e.ctx, store.LocalHost, true, "alice"))
	if h, ok := e.reg.Get(store.LocalHost); !ok || h.Name != "here" {
		t.Errorf("the local host is not back: %+v", h)
	}
	if err := e.mgr.SetEnabled(e.ctx, 99, true, "alice"); err == nil {
		t.Error("enabled an unknown host")
	}

	// Owed starts can be forgotten.
	run, _ := e.st.CreateRun(e.ctx, "auto", 0, time.Now())
	must(e.st.AddRunAction(e.ctx, run.ID, store.RunAction{HostID: id, ContainerID: "id-db", Key: "media/db", Name: "media-db-1", WasRunning: true}))
	must(e.st.MarkPending(e.ctx, run.ID, id, "id-db", "waiting"))
	must(e.mgr.ForgetPending(e.ctx, id, "alice"))
	if p, _ := e.st.PendingStarts(e.ctx); len(p) != 0 {
		t.Errorf("still pending: %+v", p)
	}

	must(e.mgr.Remove(e.ctx, id, "alice"))
	if _, ok := e.reg.Get(id); ok {
		t.Error("a removed host is still in the registry")
	}
	if _, ok, _ := e.st.Agent(e.ctx, id); ok {
		t.Error("a removed host is still stored")
	}
	if err := e.mgr.Remove(e.ctx, id, "alice"); err == nil {
		t.Error("removed twice")
	}
	if _, err := e.mgr.NewToken(e.ctx, id, "alice"); err == nil {
		t.Error("a token for a removed host")
	}
	for _, typ := range []string{"host_renamed", "host_disabled", "host_enabled", "pending_forgotten", "host_removed"} {
		if len(e.events(typ)) == 0 {
			t.Errorf("no %s event", typ)
		}
	}
}

// post sends a JSON body to the agent API, signed with key when it is set.
func (e *env) post(key *agentauth.Key, host int64, path string, body any) (int, string) {
	e.t.Helper()
	raw, _ := json.Marshal(body)
	if s, ok := body.(string); ok {
		raw = []byte(s)
	}
	req, _ := http.NewRequestWithContext(e.ctx, http.MethodPost, e.url+path, bytes.NewReader(raw))
	if key != nil {
		auth, err := agentauth.SignRequest(*key, host, http.MethodPost, path, raw, time.Now())
		if err != nil {
			e.t.Fatal(err)
		}
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(resp.Body) //nolint:errcheck // for messages only
	return resp.StatusCode, b.String()
}

func TestEnrollRefusals(t *testing.T) {
	e := newEnv(t)
	k, _ := agentauth.NewKey()
	_, token, err := e.mgr.Add(e.ctx, "nas", "alice")
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		body any
		want int
	}{
		"not json":    {"{", http.StatusBadRequest},
		"bad key":     {agentauth.EnrollRequest{Token: token, PublicKey: "rsa:x"}, http.StatusBadRequest},
		"wrong token": {agentauth.EnrollRequest{Token: "dbe_nope", PublicKey: k.Public().String()}, http.StatusUnauthorized},
	} {
		if code, _ := e.post(nil, 0, agentauth.PathEnroll, c.body); code != c.want {
			t.Errorf("%s: %d, want %d", name, code, c.want)
		}
	}
	// An expired token.
	e.mgr.Now = func() time.Time { return time.Now().Add(store.EnrollTokenTTL + time.Minute) }
	if code, body := e.post(nil, 0, agentauth.PathEnroll, agentauth.EnrollRequest{Token: token, PublicKey: k.Public().String()}); code != http.StatusUnauthorized || !strings.Contains(body, "expired") {
		t.Errorf("expired token: %d %s", code, body)
	}
	// Too many attempts from one address.
	e.mgr.Now = time.Now
	code := 0
	for range 12 {
		code, _ = e.post(nil, 0, agentauth.PathEnroll, "{")
	}
	if code != http.StatusTooManyRequests {
		t.Errorf("after many attempts: %d", code)
	}
}

func TestAPIRefusals(t *testing.T) {
	e := newEnv(t)
	id, dir := e.startAgent(newDocker())
	b, _ := os.ReadFile(filepath.Join(dir, "key"))
	key, _ := agentauth.ParseKey(string(b))
	other, _ := agentauth.NewKey()

	if code, _ := e.post(nil, id, agentauth.PathInventory, agentauth.Inventory{}); code != http.StatusUnauthorized {
		t.Errorf("unsigned: %d", code)
	}
	if code, _ := e.post(&other, id, agentauth.PathInventory, agentauth.Inventory{}); code != http.StatusUnauthorized {
		t.Errorf("signed with another key: %d", code)
	}
	if len(e.events("agent_refused")) != 1 {
		t.Error("a bad signature was not recorded")
	}
	e.post(&other, id, agentauth.PathInventory, agentauth.Inventory{})
	if len(e.events("agent_refused")) != 1 {
		t.Error("agent_refused is recorded at most once an hour")
	}
	if code, _ := e.post(&key, 99, agentauth.PathInventory, agentauth.Inventory{}); code != http.StatusUnauthorized {
		t.Errorf("unknown host: %d", code)
	}
	for _, p := range []string{agentauth.PathInventory, agentauth.PathResults, agentauth.PathRotate} {
		if code, _ := e.post(&key, id, p, "{"); code != http.StatusBadRequest {
			t.Errorf("%s with a bad body: %d", p, code)
		}
	}

	// A replayed request.
	raw := []byte(`{"version":"test","platform":"docker","workloads":[]}`)
	auth, _ := agentauth.SignRequest(key, id, http.MethodPost, agentauth.PathInventory, raw, time.Now())
	for i, want := range []int{http.StatusNoContent, http.StatusUnauthorized} {
		req, _ := http.NewRequestWithContext(e.ctx, http.MethodPost, e.url+agentauth.PathInventory, bytes.NewReader(raw))
		req.Header.Set("Authorization", auth)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("request %d: %d, want %d", i, resp.StatusCode, want)
		}
	}

	// A disabled host is told so.
	if err := e.mgr.SetEnabled(e.ctx, id, false, "alice"); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.post(&key, id, agentauth.PathInventory, agentauth.Inventory{}); code != http.StatusForbidden {
		t.Errorf("disabled host: %d", code)
	}
	if err := e.mgr.SetEnabled(e.ctx, id, true, "alice"); err != nil {
		t.Fatal(err)
	}

	// A key past MaxKeyAge is refused. The key is made old in the store
	// rather than by moving the manager's clock, which the running agent
	// reads too.
	if err := e.st.RotateAgentKey(e.ctx, id, key.Public().String(), time.Now().Add(-agents.MaxKeyAge-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if code, body := e.signedAt(key, id, agentauth.PathCommands+"?wait=0", time.Now()); code != http.StatusUnauthorized || !strings.Contains(body, "older than 90 days") {
		t.Errorf("old key: %d %s", code, body)
	}
}

// signedAt makes a GET signed at a given time.
func (e *env) signedAt(key agentauth.Key, host int64, path string, at time.Time) (int, string) {
	e.t.Helper()
	auth, _ := agentauth.SignRequest(key, host, http.MethodGet, path, nil, at)
	req, _ := http.NewRequestWithContext(e.ctx, http.MethodGet, e.url+path, nil)
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(resp.Body) //nolint:errcheck // for messages only
	return resp.StatusCode, b.String()
}

func TestRotateRefusals(t *testing.T) {
	e := newEnv(t)
	id, dir := e.startAgent(newDocker())
	b, _ := os.ReadFile(filepath.Join(dir, "key"))
	key, _ := agentauth.ParseKey(string(b))
	next, _ := agentauth.NewKey()
	ts := time.Now().Unix()
	proof := next.Sign(agentauth.RotationProof(id, next.Public(), ts))
	for name, c := range map[string]struct {
		req  agentauth.RotateRequest
		want int
	}{
		"bad key":     {agentauth.RotateRequest{NewKey: "x", TS: ts, Proof: proof}, http.StatusBadRequest},
		"bad proof":   {agentauth.RotateRequest{NewKey: next.Public().String(), TS: ts, Proof: key.Sign([]byte("x"))}, http.StatusBadRequest},
		"old proof":   {agentauth.RotateRequest{NewKey: next.Public().String(), TS: ts - 3600, Proof: next.Sign(agentauth.RotationProof(id, next.Public(), ts-3600))}, http.StatusBadRequest},
		"current key": {agentauth.RotateRequest{NewKey: key.Public().String(), TS: ts}, http.StatusNoContent},
	} {
		if code, body := e.post(&key, id, agentauth.PathRotate, c.req); code != c.want {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	// A rotation signed with the key it replaced is refused, but its repeat
	// (the same new key) is accepted.
	if code, _ := e.post(&key, id, agentauth.PathRotate, agentauth.RotateRequest{NewKey: next.Public().String(), TS: ts, Proof: proof}); code != http.StatusNoContent {
		t.Fatalf("rotation: %d", code)
	}
	third, _ := agentauth.NewKey()
	p3 := third.Sign(agentauth.RotationProof(id, third.Public(), ts))
	if code, _ := e.post(&key, id, agentauth.PathRotate, agentauth.RotateRequest{NewKey: third.Public().String(), TS: ts, Proof: p3}); code != http.StatusUnauthorized {
		t.Errorf("rotation signed with the previous key: %d", code)
	}
	if code, _ := e.post(&key, id, agentauth.PathRotate, agentauth.RotateRequest{NewKey: next.Public().String(), TS: ts, Proof: proof}); code != http.StatusNoContent {
		t.Errorf("repeated rotation: %d", code)
	}
}

func TestLoadAndKeyWarning(t *testing.T) {
	e := newEnv(t)
	// Enrolled by hand: no agent pushes an inventory over the saved one.
	_, token, err := e.mgr.Add(e.ctx, "nas", "alice")
	if err != nil {
		t.Fatal(err)
	}
	key, _ := agentauth.NewKey()
	if code, body := e.post(nil, 0, agentauth.PathEnroll, agentauth.EnrollRequest{Token: token, PublicKey: key.Public().String()}); code != http.StatusOK {
		t.Fatalf("enroll: %d %s", code, body)
	}
	id := e.agentRow(2).ID
	if err := e.st.SaveInventory(e.ctx, id, `[{"id":"x","name":"saved","kind":"container","running":true}]`, "docker", "test"); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetHostEnabled(e.ctx, store.LocalHost, false); err != nil {
		t.Fatal(err)
	}
	// A new manager, as after a restart.
	reg := hosts.NewRegistry(hosts.Host{ID: store.LocalHost, Name: "local", Runtime: hosts.Local{Docker: newDocker()}})
	m2 := agents.New(e.st, nil, reg, hosts.Host{}, e.log)
	m2.Secrets = e.mgr.Secrets
	if err := m2.Load(e.ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get(store.LocalHost); ok {
		t.Error("the disabled local host was kept")
	}
	h, ok := reg.Get(id)
	if !ok {
		t.Fatal("the agent host was not loaded")
	}
	if ws, err := h.Runtime.List(e.ctx); err != nil || len(ws) != 1 || ws[0].Name != "saved" {
		t.Errorf("stored inventory = %+v, %v", ws, err)
	}

	// A key close to MaxKeyAge is warned about once.
	m2.Now = func() time.Time { return time.Now().Add(agents.MaxKeyAge - agents.KeyWarning + time.Hour) }
	go m2.Watch(e.ctx, 5*time.Millisecond)
	e.waitFor("host_key_expiring", func() bool { return len(e.events("host_key_expiring")) == 1 })
	time.Sleep(30 * time.Millisecond)
	if n := len(e.events("host_key_expiring")); n != 1 {
		t.Errorf("warned %d times", n)
	}
}
