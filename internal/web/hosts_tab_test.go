// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/agentauth"
	"github.com/zjcochran/wan-drawbridge/internal/store"
)

var tokenRE = regexp.MustCompile(`DRAWBRIDGE_ENROLL_TOKEN: (dbe_[A-Za-z0-9_-]+)`)

// enrolledHost adds "nas" in Settings → Hosts and enrolls an agent for it
// through the web handler.
func enrolledHost(t *testing.T, e *env, c *client) (agentauth.Key, agentauth.EnrollResponse) {
	t.Helper()
	_, page := c.get("/settings/hosts")
	if !strings.Contains(page, "<strong>local</strong>") || !strings.Contains(page, "Add a host") || !strings.Contains(page, "ed25519:") {
		t.Fatalf("hosts tab:\n%s", page)
	}
	resp, body := c.htmx("/settings/hosts", url.Values{"name": {"nas"}})
	m := tokenRE.FindStringSubmatch(body)
	if resp.StatusCode != http.StatusOK || m == nil || !strings.Contains(body, "DRAWBRIDGE_MANAGER: http://") ||
		!strings.Contains(body, "Waiting for the agent") || !strings.Contains(body, "command: agent") {
		t.Fatalf("add host: %d\n%s", resp.StatusCode, body)
	}
	if resp, _ := c.htmx("/settings/hosts", url.Values{"name": {"nas"}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("duplicate host: %d", resp.StatusCode)
	}
	if _, again := c.get("/settings/hosts"); strings.Contains(again, m[1]) {
		t.Error("the token is shown again")
	}

	// The agent API is mounted: enrolling with the token works.
	key, _ := agentauth.NewKey()
	raw, _ := json.Marshal(agentauth.EnrollRequest{Token: m[1], PublicKey: key.Public().String(), Platform: "docker", Version: "v9"})
	req := httptest.NewRequest(http.MethodPost, agentauth.PathEnroll, bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	e.ui.Handler().ServeHTTP(rec, req)
	var er agentauth.EnrollResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &er) != nil || er.Name != "nas" {
		t.Fatalf("enroll through the web handler: %d %s", rec.Code, rec.Body)
	}
	return key, er
}

func TestHostsTab(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	key, _ := enrolledHost(t, e, c)
	_, page := c.get("/settings/hosts")
	if !strings.Contains(page, "Online") || !strings.Contains(page, "key "+key.Public().Fingerprint()) ||
		!strings.Contains(page, "not the manager's version") || !strings.Contains(page, "0 days old; rotates in") {
		t.Fatalf("enrolled host:\n%s", page)
	}
}

func TestHostOps(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	ctx := context.Background()
	_, er := enrolledHost(t, e, c)
	id := strconv.FormatInt(er.HostID, 10)

	for _, op := range []struct {
		path string
		form url.Values
		want string
	}{
		{"/settings/hosts/" + id + "/rename", url.Values{"name": {"attic"}}, "<strong>attic</strong>"},
		{"/settings/hosts/" + id + "/rotate", nil, "<strong>attic</strong>"},
		{"/settings/hosts/" + id + "/disable", nil, "Disabled"},
		{"/settings/hosts/" + id + "/enable", nil, "<strong>attic</strong>"},
		{"/settings/hosts/" + id + "/revoke", nil, "Revoked"},
		{"/settings/hosts/" + id + "/token", nil, "DRAWBRIDGE_ENROLL_TOKEN: dbe_"},
		{"/settings/hosts/manager-key", nil, "ed25519:"},
	} {
		if resp, body := c.htmx(op.path, op.form); resp.StatusCode != http.StatusOK || !strings.Contains(body, op.want) {
			t.Errorf("%s: %d\n%s", op.path, resp.StatusCode, body)
		}
	}

	// Owed starts are shown and can be forgotten.
	run, _ := e.st.CreateRun(ctx, "auto", 0, time.Now())
	e.st.AddRunAction(ctx, run.ID, store.RunAction{HostID: er.HostID, ContainerID: "c", Key: "media/db", Name: "media-db-1", WasRunning: true})
	e.st.MarkPending(ctx, run.ID, er.HostID, "c", "waiting")
	if _, page := c.get("/settings/hosts"); !strings.Contains(page, "1 container to start once it is back") {
		t.Errorf("owed starts:\n%s", page)
	}
	if resp, body := c.htmx("/settings/hosts/"+id+"/forget", nil); resp.StatusCode != http.StatusOK || strings.Contains(body, "to start once it is back") {
		t.Errorf("forget: %d", resp.StatusCode)
	}
	if resp, body := c.htmx("/settings/hosts/"+id+"/remove", nil); resp.StatusCode != http.StatusOK || strings.Contains(body, "attic") {
		t.Errorf("remove: %d", resp.StatusCode)
	}

	for path, want := range map[string]int{
		"/settings/hosts/x/rename":   http.StatusNotFound,
		"/settings/hosts/1/explode":  http.StatusNotFound,
		"/settings/hosts/99/revoke":  http.StatusUnprocessableEntity,
		"/settings/hosts/1/remove":   http.StatusUnprocessableEntity,
		"/settings/hosts/1/rename":   http.StatusUnprocessableEntity, // no name
		"/settings/hosts/99/disable": http.StatusUnprocessableEntity,
	} {
		if resp, _ := c.htmx(path, nil); resp.StatusCode != want {
			t.Errorf("%s: %d, want %d", path, resp.StatusCode, want)
		}
	}
}

func TestHostsTabForViewers(t *testing.T) {
	e := newEnv(t)
	e.admin()
	v := e.viewerClient()
	_, page := v.get("/settings/hosts")
	if !strings.Contains(page, "<strong>local</strong>") || strings.Contains(page, "Add a host") || strings.Contains(page, "ed25519:") ||
		strings.Contains(page, "hx-post=\"/settings/hosts") {
		t.Errorf("viewers must see hosts read-only:\n%s", page)
	}
	if resp, _ := v.htmx("/settings/hosts", url.Values{"name": {"nas"}}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("viewer add: %d", resp.StatusCode)
	}
}

func TestHostsWithoutAgents(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	e.ui.Agents = nil
	if _, page := c.get("/settings/hosts"); strings.Contains(page, "Add a host") {
		t.Error("hosts can be added without the agent API")
	}
	for _, p := range []string{"/settings/hosts", "/settings/hosts/1/rename", "/settings/hosts/manager-key"} {
		if resp, _ := c.htmx(p, url.Values{"name": {"x"}}); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s without agents: %d", p, resp.StatusCode)
		}
	}
	if got := keyAge(time.Time{}, time.Now()); got != "" {
		t.Errorf("key age of nothing = %q", got)
	}
	if got := keyAge(time.Now().Add(-40*24*time.Hour), time.Now()); !strings.Contains(got, "overdue") {
		t.Errorf("old key = %q", got)
	}
}
