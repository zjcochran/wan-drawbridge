// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
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

	"github.com/zjcochran/wan-drawbridge/internal/store"
	"github.com/zjcochran/wan-drawbridge/internal/wan"
)

func TestAPITokensInSettings(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	resp, body := c.htmx("/settings/api-tokens", url.Values{"name": {"Home Assistant"}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "not shown again") {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	token := regexp.MustCompile(`dbt_[A-Za-z0-9_-]+`).FindString(body)
	if token == "" {
		t.Fatal("the new token is not shown")
	}
	if resp, _ := c.htmx("/settings/api-tokens", url.Values{"name": {""}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("empty name: %d", resp.StatusCode)
	}
	_, page := c.get("/settings/api")
	if !strings.Contains(page, "<strong>Home Assistant</strong>") || strings.Contains(page, token) {
		t.Error("the tab should list the token by name only")
	}
	tokens, _ := e.st.APITokens(context.Background())
	id := tokens[0].ID
	if resp, body := c.htmx("/settings/api-tokens/"+itoa64(id)+"/delete", url.Values{}); resp.StatusCode != http.StatusOK || strings.Contains(body, "<strong>Home Assistant</strong>") {
		t.Errorf("revoke: %d", resp.StatusCode)
	}
	if resp, _ := c.htmx("/settings/api-tokens/"+itoa64(id)+"/delete", url.Values{}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("revoke twice: %d", resp.StatusCode)
	}
	if events, _ := e.st.Events(context.Background(), store.EventFilter{Type: "api_token_created"}); len(events) != 1 {
		t.Errorf("api_token_created events: %d", len(events))
	}
}

func apiGet(t *testing.T, e *env, token string) (*http.Response, apiStatus) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.ui.Handler().ServeHTTP(rec, req)
	var out apiStatus
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Result(), out
}

func TestAPIStatus(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	token, err := e.st.CreateAPIToken(ctx, "test", "admin", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "dbt_nope"} {
		if resp, _ := apiGet(t, e, bad); resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("token %q: %d", bad, resp.StatusCode)
		}
	}

	// A failover on an ISP that is metered, with one container stopped.
	backup, _, _ := e.st.SeeASN(ctx, 64510, "SKYLINK-AS64510", time.Now())
	e.mon.state, e.mon.isp = wan.Failover, backup
	run, _ := e.st.CreateRun(ctx, "auto", 64510, time.Now())
	e.st.AddRunAction(ctx, run.ID, store.RunAction{ContainerID: "c1", Key: "media/player", Name: "media-player-1", WasRunning: true})
	e.st.MarkStopped(ctx, run.ID, "c1", "", time.Now())

	resp, st := apiGet(t, e, token)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("status: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	checkFailoverStatus(t, st)
	if tokens, _ := e.st.APITokens(ctx); tokens[0].LastUsed.IsZero() {
		t.Error("the use was not recorded")
	}

	// Pending, on the primary.
	e.mon.state, e.mon.isp = wan.FailbackPending, 0
	if _, st = apiGet(t, e, token); st.Due == nil || st.CurrentISP.Metered == nil || *st.CurrentISP.Metered {
		t.Errorf("pending status = %+v", st)
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

func checkFailoverStatus(t *testing.T, st apiStatus) {
	t.Helper()
	if st.State != "failover" || st.StateLabel != "Failover" || st.Due != nil || st.LastCheck == nil {
		t.Errorf("state = %+v", st)
	}
	if st.CurrentISP == nil || st.CurrentISP.Name != "SKYLINK" || st.CurrentISP.Metered == nil || !*st.CurrentISP.Metered {
		t.Errorf("current ISP = %+v", st.CurrentISP)
	}
	if st.PrimaryISP == nil || st.PrimaryISP.ID != 1 || len(st.Stopped) != 1 || st.Stopped[0] != "media-player-1" {
		t.Errorf("primary = %+v, stopped = %v", st.PrimaryISP, st.Stopped)
	}
}
