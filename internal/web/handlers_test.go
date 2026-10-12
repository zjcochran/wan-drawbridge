// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/store"
	"github.com/zjcochran/wan-drawbridge/internal/wan"
)

func TestISPManagement(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	ctx := context.Background()
	tmo, _, _ := e.st.SeeASN(ctx, 64510, "SKYLINK-AS64510", time.Now())

	if resp, body := c.htmx("/isps/1/rename", url.Values{"name": {"Home cable"}}); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Home cable") {
		t.Fatalf("rename: %d %s", resp.StatusCode, body)
	}
	if resp, _ := c.htmx("/isps/999/rename", url.Values{"name": {"x"}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("rename unknown ISP: %d", resp.StatusCode)
	}

	if resp, _ := c.htmx("/isps/1/role", url.Values{"role": {"primary"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("set role: %d", resp.StatusCode)
	}
	if id, pinned, _ := e.st.Primary(ctx); id != 1 || !pinned {
		t.Fatalf("primary = %d pinned=%v", id, pinned)
	}
	if resp, _ := c.htmx("/isps/1/role", url.Values{"role": {"owner"}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("bad role: %d", resp.StatusCode)
	}

	if resp, _ := c.htmx("/asns/64510/move", url.Values{"to": {"1"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("move to ISP 1: %d", resp.StatusCode)
	}
	if isp, _ := e.st.ISPOfASN(ctx, 64510); isp != 1 {
		t.Fatalf("AS64510 is in ISP %d after the move", isp)
	}
	if resp, _ := c.htmx("/asns/64510/move", url.Values{"to": {"new"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("move to a new ISP: %d", resp.StatusCode)
	}
	if isp, _ := e.st.ISPOfASN(ctx, 64510); isp == 1 || isp == tmo {
		t.Fatalf("AS64510 did not get its own group: %d", isp)
	}
	for _, bad := range []struct{ asn, to string }{{"64510", "x"}, {"64510", "0"}, {"64512", "1"}} {
		if resp, _ := c.htmx("/asns/"+bad.asn+"/move", url.Values{"to": {bad.to}}); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("move AS%s to %q: %d", bad.asn, bad.to, resp.StatusCode)
		}
	}

	if resp, _ := c.htmx("/isps/relearn", url.Values{}); resp.StatusCode != http.StatusOK {
		t.Fatalf("relearn: %d", resp.StatusCode)
	}
	e.mon.failErr = errors.New("monitor busy")
	if resp, _ := c.htmx("/isps/relearn", url.Values{}); resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("relearn failure: %d", resp.StatusCode)
	}
}

func TestBulkContainerPolicy(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	resp, body := c.htmx("/containers/bulk", url.Values{"keys": {"media/player", "dns"}, "bulk_policy": {"disable"}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "media-player-1") {
		t.Fatalf("bulk: %d", resp.StatusCode)
	}
	rows, _ := e.st.Containers(context.Background())
	for _, r := range rows {
		want := store.Inherit
		if r.Key == "media/player" || r.Key == "dns" {
			want = store.Disable
		}
		if r.Policy != want {
			t.Errorf("%s policy %s, want %s", r.Key, r.Policy, want)
		}
	}
	if e.mon.policies == 0 {
		t.Error("the monitor was not told about the policy change")
	}
	if resp, _ := c.htmx("/containers/bulk", url.Values{"bulk_policy": {"disable"}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("bulk with nothing selected: %d", resp.StatusCode)
	}
	if resp, _ := c.htmx("/containers/bulk", url.Values{"keys": {"dns"}, "bulk_policy": {"sometimes"}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("bulk with a bad policy: %d", resp.StatusCode)
	}
}

func TestChangePassword(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	for _, bad := range []url.Values{
		{"current": {"wrong password!!"}, "new": {"another long password"}, "confirm": {"another long password"}},
		{"current": {"test-password-123"}, "new": {"another long password"}, "confirm": {"different password!"}},
		{"current": {"test-password-123"}, "new": {"short"}, "confirm": {"short"}},
	} {
		if resp, _ := c.htmx("/settings/password", bad); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%v: %d", bad, resp.StatusCode)
		}
	}
	resp, _ := c.htmx("/settings/password", url.Values{
		"current": {"test-password-123"}, "new": {"another long password"}, "confirm": {"another long password"}})
	if resp.StatusCode != http.StatusNoContent || !strings.HasPrefix(resp.Header.Get("HX-Redirect"), "/login") {
		t.Fatalf("change: %d %s", resp.StatusCode, resp.Header.Get("HX-Redirect"))
	}
	if resp, _ := c.do("GET", "/settings", nil); resp.StatusCode != http.StatusSeeOther {
		t.Error("the old session should have ended")
	}
	if _, err := e.st.Authenticate(context.Background(), "admin", "another long password", time.Now()); err != nil {
		t.Errorf("new password does not work: %v", err)
	}
}

func TestUserOperations(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	if resp, body := c.htmx("/users", url.Values{"username": {"amy"}, "password": {"test-password-123"}, "role": {"viewer"}}); resp.StatusCode != http.StatusOK || !strings.Contains(body, "amy") {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	if resp, _ := c.htmx("/users", url.Values{"username": {"amy"}, "password": {"test-password-123"}, "role": {"viewer"}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("duplicate user: %d", resp.StatusCode)
	}
	users, _ := e.st.Users(context.Background())
	var amy, me int
	for _, u := range users {
		if u.Username == "amy" {
			amy = u.ID
		} else {
			me = u.ID
		}
	}
	id := func(n int) string { return "/users/" + strconv.Itoa(n) }

	if resp, _ := c.htmx(id(amy)+"/disable", url.Values{}); resp.StatusCode != http.StatusOK {
		t.Fatalf("disable: %d", resp.StatusCode)
	}
	if u, _ := e.st.UserByID(context.Background(), amy); !u.Disabled {
		t.Fatal("not disabled")
	}
	if resp, _ := c.htmx(id(amy)+"/enable", url.Values{}); resp.StatusCode != http.StatusOK {
		t.Fatalf("enable: %d", resp.StatusCode)
	}
	if resp, _ := c.htmx(id(amy)+"/password", url.Values{"password": {"a brand new password"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("reset password: %d", resp.StatusCode)
	}
	if _, err := e.st.Authenticate(context.Background(), "amy", "a brand new password", time.Now()); err != nil {
		t.Errorf("reset password does not work: %v", err)
	}
	if resp, _ := c.htmx(id(amy)+"/password", url.Values{"password": {"short"}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("short password: %d", resp.StatusCode)
	}
	if resp, _ := c.htmx(id(me)+"/disable", url.Values{}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("disabling yourself: %d", resp.StatusCode)
	}
	if resp, _ := c.htmx(id(amy)+"/promote", url.Values{}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown operation: %d", resp.StatusCode)
	}
}

func TestLogout(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	resp, _ := c.do("POST", "/logout", url.Values{"_csrf": {c.csrf}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("logout: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := c.do("GET", "/settings", nil); resp.StatusCode != http.StatusSeeOther {
		t.Error("still signed in after logout")
	}
}

func TestSetupDetectionAndCheck(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	form := url.Values{}
	for k, v := range map[string]string{"check_interval": "30s", "failover_after": "1m", "failback_after": "10m",
		"failback_max_hold": "2h", "quorum": "2", "timeout": "5s", "learning_period": "24h", "retention": "30d",
		"ip_providers": "https://a.example\nhttps://b.example"} {
		form.Set(k, v)
	}
	resp, _ := c.htmx("/setup/detection", form)
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("HX-Redirect") != "/setup?step=5" {
		t.Fatalf("detection step: %d %s", resp.StatusCode, resp.Header.Get("HX-Redirect"))
	}
	if e.mon.det.CheckInterval != 30*time.Second || e.mon.det.FailoverAfter != time.Minute {
		t.Fatalf("detection not applied: %+v", e.mon.det)
	}
	form.Set("check_interval", "soon")
	if resp, _ := c.htmx("/setup/detection", form); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("bad duration: %d", resp.StatusCode)
	}
	form.Set("check_interval", "1s")
	if resp, _ := c.htmx("/setup/detection", form); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("out of range: %d", resp.StatusCode)
	}

	if resp, body := c.htmx("/setup/check", url.Values{}); resp.StatusCode != http.StatusOK || !strings.Contains(body, "203.0.113.7") {
		t.Fatalf("check: %d %s", resp.StatusCode, body)
	}
	e.mon.failErr = errors.New("no providers answered")
	if resp, _ := c.htmx("/setup/check", url.Values{}); resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("failed check: %d", resp.StatusCode)
	}
	if resp, _ := c.htmx("/setup/primary", url.Values{"choice": {"pin"}, "isp": {"0"}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("pin without an ISP: %d", resp.StatusCode)
	}
	if resp, _ := c.htmx("/setup/primary", url.Values{"choice": {"learn"}}); resp.StatusCode != http.StatusNoContent {
		t.Errorf("learn: %d", resp.StatusCode)
	}
}

func TestUnlinkSSO(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	ctx := context.Background()
	u, _ := e.st.Authenticate(ctx, "admin", "test-password-123", time.Now())
	e.st.LinkIdentity(ctx, u.ID, "https://id.example", "sub-1", "", time.Now())
	resp, _ := c.htmx("/settings/sso/unlink", url.Values{})
	if resp.StatusCode != http.StatusNoContent || !strings.Contains(resp.Header.Get("HX-Redirect"), "unlinked") {
		t.Fatalf("unlink: %d %s", resp.StatusCode, resp.Header.Get("HX-Redirect"))
	}
	if ids, _ := e.st.Identities(ctx, u.ID); len(ids) != 0 {
		t.Fatalf("identities left: %+v", ids)
	}
}

func TestDashboardInEveryState(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	e.st.SetSetting(context.Background(), store.SettingSetupComplete, true)
	for state, want := range map[wan.State]string{
		wan.Learning:        "Learning",
		wan.Primary:         "On primary",
		wan.FailoverPending: "Failover pending",
		wan.Failover:        "Failover",
		wan.FailbackPending: "Failback pending",
	} {
		e.mon.state = state
		for _, path := range []string{"/", "/partials/status"} {
			if resp, body := c.get(path); resp.StatusCode != http.StatusOK || !strings.Contains(body, want) {
				t.Errorf("%s in %s: %d, missing %q", path, state, resp.StatusCode, want)
			}
		}
	}
}

func TestServerSentEvents(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.srv.URL+"/sse", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" || resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("headers: %v", resp.Header)
	}
	r := bufio.NewReader(resp.Body)
	if line, _ := r.ReadString('\n'); line != ": connected\n" {
		t.Fatalf("first line %q", line)
	}
	r.ReadString('\n')
	e.ui.Hub.Publish(TopicState)
	if line, _ := r.ReadString('\n'); line != "event: "+TopicState+"\n" {
		t.Fatalf("event line %q", line)
	}
}

func TestStaticFilesAreCached(t *testing.T) {
	e := newEnv(t)
	resp, _ := e.client().do("GET", "/static/app.css", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Cache-Control"), "max-age") {
		t.Fatalf("static: %d %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
}

func TestDatabaseFailureShowsError(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	e.st.SetSetting(context.Background(), store.SettingSetupComplete, true)
	c.get("/users")
	csrf := c.csrf
	e.st.Close()
	// The session lookup itself fails, so every page reports the error.
	for _, path := range []string{"/", "/stacks", "/containers", "/isps", "/events", "/users", "/settings"} {
		if resp, _ := c.do("GET", path, nil); resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("%s with the database gone: %d", path, resp.StatusCode)
		}
	}
	c.csrf = csrf
	if resp, _ := c.htmx("/isps/relearn", url.Values{}); resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("action with the database gone: %d", resp.StatusCode)
	}
}

func TestFormatHelpers(t *testing.T) {
	now := time.Now()
	for d, want := range map[time.Duration]string{
		0: "a moment", -time.Minute: "a moment", 45 * time.Second: "45s",
		3*time.Minute + 20*time.Second: "3m 20s", 2*time.Hour + 5*time.Minute: "2h 05m",
	} {
		at := now.Add(d)
		if d == 0 {
			at = time.Time{}
		}
		if got := until(at); got != want {
			t.Errorf("until(+%s) = %q, want %q", d, got, want)
		}
	}
	for d, want := range map[time.Duration]string{
		time.Second: "just now", 30 * time.Second: "30s ago", 5 * time.Minute: "5m ago",
		3*time.Hour + 7*time.Minute: "3h 7m ago", 72 * time.Hour: "3d ago",
	} {
		if got := ago(now.Add(-d)); got != want {
			t.Errorf("ago(-%s) = %q, want %q", d, got, want)
		}
	}
	if ago(time.Time{}) != "never" || zeroOr(time.Time{}, "x") != "never" || zeroOr(now, "x") != "x" {
		t.Error("zero times should read never")
	}
	if pct(1, 4) != "25%" || pct(1, 0) != "–" {
		t.Error("pct")
	}
	for s, want := range map[wan.State]string{wan.Primary: "ok", wan.FailoverPending: "warn",
		wan.FailbackPending: "warn", wan.Failover: "bad", wan.Learning: "muted"} {
		if stateClass(s) != want {
			t.Errorf("stateClass(%s) = %s", s, stateClass(s))
		}
	}
	if stateLabel("") != "Starting" {
		t.Error("unknown state label")
	}
}

// breakTables drops tables behind the store's back, so handlers hit database
// errors after sign-in (which needs only users and sessions) has succeeded.
func (e *env) breakTables(tables ...string) {
	e.t.Helper()
	db, err := sql.Open("sqlite", "file:"+e.dbPath+"?_pragma=foreign_keys(0)")
	if err != nil {
		e.t.Fatal(err)
	}
	defer db.Close()
	for _, tbl := range tables {
		// Test-only: tbl is one of the fixed table names passed by the test.
		// nosemgrep
		if _, err := db.Exec("DROP TABLE " + tbl); err != nil {
			e.t.Fatal(err)
		}
	}
}

func TestHandlersReportDatabaseErrors(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	e.st.SetSetting(context.Background(), store.SettingSetupComplete, true)
	c.get("/settings")
	e.breakTables("run_actions", "runs", "containers", "stacks", "checks", "asn_daily", "asns", "isps", "events", "oidc_identities")
	for _, path := range []string{"/", "/partials/status", "/partials/timeline", "/stacks", "/stacks/media/containers",
		"/containers", "/isps", "/events", "/events.csv", "/settings"} {
		if resp, _ := c.do("GET", path, nil); resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("GET %s: %d", path, resp.StatusCode)
		}
	}
	for path, form := range map[string]url.Values{
		"/stacks/media/policy": {"disable": {"on"}},
		"/containers/policy":   {"key": {"dns"}, "policy": {"disable"}},
		"/isps/1/role":         {"role": {"primary"}},
		"/isps/1/rename":       {"name": {"x"}},
		"/asns/64500/move":     {"to": {"new"}},
		"/setup/check":         {},
		"/setup/primary":       {"choice": {"pin"}, "isp": {"1"}},
		"/settings/sso/unlink": {},
		"/actions/failover":    {},
		"/settings/sso":        {"issuer": {"https://id.example"}, "client_id": {"x"}},
	} {
		if resp, _ := c.htmx(path, form); resp.StatusCode < 400 {
			t.Errorf("POST %s: %d", path, resp.StatusCode)
		}
	}
}

func TestTimelineSegments(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	ctx := context.Background()
	e.st.SetRole(ctx, 1, store.RolePrimary, true)
	e.st.SeeASN(ctx, 64510, "SKYLINK-AS64510", time.Now())
	e.mon.det.CheckInterval = time.Minute
	now := time.Now()
	for i, asn := range []int{64500, 64500, 64500, 64510, 64510, 0, 64500} {
		at := now.Add(time.Duration(i-30) * time.Minute)
		if i == 6 {
			at = now.Add(-time.Minute) // after a gap: no data in between
		}
		e.st.RecordCheck(ctx, store.Check{TS: at, ASN: asn, Conclusive: asn != 0})
	}
	resp, body := c.get("/partials/timeline")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("timeline: %d", resp.StatusCode)
	}
	for _, want := range []string{`class="seg-primary"`, `class="seg-backup"`, `class="seg-inconclusive"`, "Primary: EXAMPLENET", "SKYLINK"} {
		if !strings.Contains(body, want) {
			t.Errorf("timeline is missing %q", want)
		}
	}
}

func TestEventsPagingAndCSV(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	ctx := context.Background()
	for i := range eventsPageSize + 5 {
		e.st.AddEvent(ctx, time.Now(), "check", map[string]any{"n": i})
	}
	e.st.AddEvent(ctx, time.Now(), "note", map[string]any{"text": "=HYPERLINK(1)"})

	_, body := c.get("/events?type=check")
	if !strings.Contains(body, "before=") {
		t.Fatal("a full page should link to the next one")
	}
	evs, _ := e.st.Events(ctx, store.EventFilter{Type: "check", Limit: 1000})
	second := evs[len(evs)-2] // n=1
	if _, body := c.get("/events?type=check&before=" + strconv.FormatInt(second.ID, 10)); strings.Contains(body, "n=1") || !strings.Contains(body, "n=0") {
		t.Fatal("paging back should show only the older events")
	}
	resp, csv := c.do("GET", "/events.csv?type=note", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("csv: %d %v", resp.StatusCode, resp.Header)
	}
	if !strings.Contains(csv, "'text==HYPERLINK(1)") && !strings.Contains(csv, "text==HYPERLINK(1)") {
		t.Fatalf("csv body: %s", csv)
	}
	for in, want := range map[string]string{"=1+1": "'=1+1", "+x": "'+x", "-x": "'-x", "@x": "'@x", "plain": "plain", "": ""} {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoginPageWhenSignedIn(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	if resp, _ := c.do("GET", "/login", nil); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("signed-in login page: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if minTime(time.Unix(1, 0), time.Unix(2, 0)) != time.Unix(1, 0) || minTime(time.Unix(2, 0), time.Unix(1, 0)) != time.Unix(1, 0) {
		t.Error("minTime")
	}
}
