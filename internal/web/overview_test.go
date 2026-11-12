// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/monitor"
	"github.com/zjcochran/wan-drawbridge/internal/store"
	"github.com/zjcochran/wan-drawbridge/internal/wan"
)

func TestDashboardOverview(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	ctx := context.Background()
	e.st.SetSetting(ctx, store.SettingSetupComplete, true)
	c.htmx("/stacks/1/media/policy", url.Values{"disable": {"on"}})
	_, page := c.get("/")
	for _, want := range []string{"All quiet", "On primary, 30 days", "100.00%", "never on a backup",
		"If the WAN fails over", "<b>2</b>", "The rules", "Last 24 hours", "Latest changes", "set the stack media to stop on failover"} {
		if !strings.Contains(page, want) {
			t.Errorf("the dashboard lacks %q", want)
		}
	}
	for rng, title := range map[string]string{"7d": "Last 7 days", "30d": "Last 30 days", "bogus": "Last 24 hours"} {
		if _, body := c.get("/partials/timeline?range=" + rng); !strings.Contains(body, title) {
			t.Errorf("range %s: no %q", rng, title)
		}
	}
}

func TestStateSummaryAndGreeting(t *testing.T) {
	for st, want := range map[wan.State]string{
		wan.Primary: "All quiet.", wan.Failover: "On SKYLINK since", wan.FailoverPending: "confirming",
		wan.FailbackPending: "making sure", wan.Learning: "Still learning",
	} {
		if got := stateSummary(statusData{S: monitorStatus(st), ISPName: "SKYLINK"}); !strings.Contains(got, want) {
			t.Errorf("%s: %q lacks %q", st, got, want)
		}
	}
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.Local)
	for h, want := range map[int]string{3: "evening", 9: "morning", 14: "afternoon", 20: "evening"} {
		if got := greeting(day.Add(time.Duration(h) * time.Hour)); !strings.Contains(got, want) {
			t.Errorf("%d h: %q", h, got)
		}
	}
}

func TestEventLines(t *testing.T) {
	ev := func(typ string, p map[string]any) store.Event { return store.Event{Type: typ, Payload: p} }
	lines := eventLines([]store.Event{
		ev("failover", map[string]any{"name": "SKYLINK", "asn": 64510.0}),
		ev("state_changed", map[string]any{"from": "primary", "to": "failover_pending", "name": "SKYLINK"}),
		ev("stack_policy", map[string]any{"by": "admin", "stack": "media", "disable": false}),
		ev("container_policy", map[string]any{"by": "admin", "container": "media/db", "policy": "keep"}),
		ev("settings_changed", map[string]any{"by": "admin"}),
		ev("user_created", map[string]any{"by": "admin", "role": "viewer", "user": "bob"}),
		ev("api_token_created", map[string]any{"by": "admin", "name": "script"}),
	}, map[int]string{})
	want := []string{"Failed over", "Failover pending", "media to keep running", "media/db to keep running",
		"changed the detection settings", "added the viewer bob", "Api token created"}
	for i, w := range want {
		if !strings.Contains(lines[i].Title, w) {
			t.Errorf("line %d: %q lacks %q", i, lines[i].Title, w)
		}
	}
	if !strings.Contains(lines[6].Text, "name=script") {
		t.Errorf("fallback text: %q", lines[6].Text)
	}
}

func TestStacksFiltersAndChanges(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	_, page := c.get("/stacks")
	for _, want := range []string{"All <span>3</span>", "Stop <span>0</span>", "Standalone", "media-player-1"} {
		if !strings.Contains(page, want) {
			t.Errorf("stacks page lacks %q", want)
		}
	}
	c.htmx("/stacks/1/media/policy", url.Values{"disable": {"on"}})
	_, body := c.htmx("/containers/policy", url.Values{"view": {"stacks"}, "container_host": {"1"}, "key": {"media/player"}, "policy": {"keep"}, "show": {"stop"}})
	if !strings.Contains(body, `id="stacks-table"`) || !strings.Contains(body, "media-indexer-1") || strings.Contains(body, "media-player-1") ||
		!strings.Contains(body, "split: 1 stop, 1 keep running") {
		t.Fatalf("change from the stacks page:\n%s", body)
	}
	if _, body := c.get("/stacks?show=review&q=nothing"); strings.Contains(body, "media-player-1") || strings.Contains(body, "Standalone") {
		t.Error("filters did not narrow the list")
	}
}

func TestISPStrip(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	ctx := context.Background()
	e.st.Observe(ctx, 64500, 86400, time.Now().AddDate(0, 0, -2))
	e.st.Observe(ctx, 64500, 60, time.Now().AddDate(0, 0, -1))
	_, page := c.get("/isps")
	if !strings.Contains(page, "Every ISP, every day") || !strings.Contains(page, `class="day-all"`) ||
		!strings.Contains(page, `class="day-part"`) || !strings.Contains(page, `class="day-none"`) {
		t.Fatalf("no strip:\n%s", page)
	}
}

func TestJump(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	if _, body := c.get("/jump?q="); !strings.Contains(body, `href="/isps"`) || !strings.Contains(body, "Settings · Users") {
		t.Errorf("empty search: %s", body)
	}
	if _, body := c.get("/jump?q=play"); !strings.Contains(body, "media-player-1") || strings.Contains(body, "Dashboard") {
		t.Errorf("container search: %s", body)
	}
	if _, body := c.get("/jump?q=media"); !strings.Contains(body, `href="/stacks?q=media%2F"`) {
		t.Errorf("stack search: %s", body)
	}
	if _, body := c.get("/jump?q=zzz"); !strings.Contains(body, "Nothing matches") {
		t.Errorf("no match: %s", body)
	}
}

func monitorStatus(s wan.State) monitor.Status { return monitor.Status{State: s, Since: time.Now()} }
