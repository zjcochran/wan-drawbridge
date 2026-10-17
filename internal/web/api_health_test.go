// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/config"
	"github.com/zjcochran/wan-drawbridge/internal/wan"
)

func healthGet(t *testing.T, e *env, token, query string) (int, apiHealth, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/health"+query, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.ui.Handler().ServeHTTP(rec, req)
	var out apiHealth
	if rec.Code == http.StatusOK || rec.Code == http.StatusServiceUnavailable {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Code, out, rec.Body.String()
}

func problemChecks(h apiHealth) string {
	var names []string
	for _, p := range h.Problems {
		names = append(names, p.Check)
	}
	return strings.Join(names, ",")
}

func healthEnv(t *testing.T) (*env, string) {
	t.Helper()
	e := newEnv(t)
	e.mon.det = config.Detection{CheckInterval: 30 * time.Second, Timeout: 5 * time.Second}
	token, err := e.st.CreateAPIToken(context.Background(), "monitor", "admin", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return e, token
}

func TestAPIHealthy(t *testing.T) {
	e, token := healthEnv(t)
	if code, _, _ := healthGet(t, e, "", ""); code != http.StatusUnauthorized {
		t.Errorf("without a token: %d", code)
	}
	code, h, _ := healthGet(t, e, token, "")
	if code != http.StatusOK || !h.Healthy || h.State != "primary" || h.Problems == nil || len(h.Problems) != 0 {
		t.Errorf("healthy: %d %+v", code, h)
	}
	if code, _, body := healthGet(t, e, token, "?ignore=nope"); code != http.StatusBadRequest || !strings.Contains(body, "failover, checks") {
		t.Errorf("unknown check: %d %s", code, body)
	}
}

func TestAPIHealthProblems(t *testing.T) {
	e, token := healthEnv(t)
	ctx := context.Background()
	id, _ := e.st.AddHost(ctx, "nas", "agent", "docker", "admin", time.Now())
	e.st.SetHostReached(ctx, id, "no answer", time.Now())
	e.mon.state, e.mon.paused, e.mon.inconcl = wan.Failover, true, true
	e.mon.lastCheck = time.Now().Add(-96 * time.Second) // 3 × 30 s + 5 s ago, and then some

	code, h, _ := healthGet(t, e, token, "")
	if code != http.StatusServiceUnavailable || h.Healthy || problemChecks(h) != "failover,checks,inconclusive,hosts,paused" {
		t.Fatalf("problems: %d %+v", code, h)
	}
	if h.Problems[3].Message != "host nas is offline" || !strings.HasPrefix(h.Problems[1].Message, "the last check was 1m3") {
		t.Errorf("messages: %+v", h.Problems)
	}
	if code, h, _ = healthGet(t, e, token, "?ignore=failover,+checks,inconclusive,hosts,paused"); code != http.StatusOK || !h.Healthy {
		t.Errorf("all ignored: %d %+v", code, h)
	}
}

func TestAPIHealthEdgeCases(t *testing.T) {
	e, token := healthEnv(t)
	// A failover test is not a problem, and nor is a failover still pending.
	e.mon.state, e.mon.drill = wan.Failover, 10*time.Minute
	if code, h, _ := healthGet(t, e, token, ""); code != http.StatusOK {
		t.Errorf("during a test: %d %+v", code, h)
	}
	e.mon.state, e.mon.drill = wan.FailoverPending, 0
	if code, _, _ := healthGet(t, e, token, ""); code != http.StatusOK {
		t.Errorf("failover pending: %d", code)
	}
	e.mon.state = wan.FailbackPending
	if _, h, _ := healthGet(t, e, token, ""); problemChecks(h) != "failover" {
		t.Errorf("failback pending: %+v", h)
	}
	// No check yet: fine just after the start, a problem once it is overdue.
	e.mon.state, e.mon.noCheck = wan.Primary, true
	if code, _, _ := healthGet(t, e, token, ""); code != http.StatusOK {
		t.Errorf("just started: %d", code)
	}
	e.ui.started = time.Now().Add(-2 * time.Minute)
	if _, h, _ := healthGet(t, e, token, ""); problemChecks(h) != "checks" || !strings.HasPrefix(h.Problems[0].Message, "no check since the start") {
		t.Errorf("never checked: %+v", h)
	}
	// A disabled host is not offline.
	ctx := context.Background()
	id, _ := e.st.AddHost(ctx, "old", "agent", "docker", "admin", time.Now())
	e.st.SetHostReached(ctx, id, "gone", time.Now())
	e.st.SetHostEnabled(ctx, id, false)
	e.ui.started = time.Now()
	if code, h, _ := healthGet(t, e, token, ""); code != http.StatusOK {
		t.Errorf("disabled host: %d %+v", code, h)
	}
}
