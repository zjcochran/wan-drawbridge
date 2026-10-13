// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/config"
	"github.com/zjcochran/wan-drawbridge/internal/docker"
	"github.com/zjcochran/wan-drawbridge/internal/monitor"
	"github.com/zjcochran/wan-drawbridge/internal/secrets"
	"github.com/zjcochran/wan-drawbridge/internal/store"
	"github.com/zjcochran/wan-drawbridge/internal/wan"
)

type fakeMon struct {
	state     wan.State // Primary when empty
	failErr   error     // returned by the action methods when set
	flaps     int
	mu        sync.Mutex
	det       config.Detection
	paused    bool
	pausedFor time.Duration
	setup     bool
	calls     []string
	policies  int
	loadErr   error // returned by LoadBackup when set
}

func (f *fakeMon) record(c string) { f.mu.Lock(); f.calls = append(f.calls, c); f.mu.Unlock() }

func (f *fakeMon) Status() monitor.Status {
	state := f.state
	if state == "" {
		state = wan.Primary
	}
	return monitor.Status{State: state, Due: time.Now().Add(90 * time.Second), Since: time.Now().Add(-3 * time.Hour), Paused: f.paused, PausedTill: f.till(), LastCheck: time.Now(), Conclusive: true,
		LastIP: "203.0.113.7", ASN: 64500, ISP: 1, PrimaryISP: 1,
		Flaps: f.flaps, Hold: (5 * time.Minute) << f.flaps}
}
func (f *fakeMon) Detection(context.Context) (config.Detection, error) { return f.det, nil }
func (f *fakeMon) ApplyDetection(_ context.Context, d config.Detection) error {
	if err := d.Validate(); err != nil {
		return err
	}
	f.det = d
	return nil
}
func (f *fakeMon) SetPaused(_ context.Context, p bool, _ string) error { f.paused = p; return nil }
func (f *fakeMon) till() time.Time {
	if f.pausedFor == 0 {
		return time.Time{}
	}
	return time.Now().Add(f.pausedFor)
}
func (f *fakeMon) PauseFor(_ context.Context, d time.Duration, _ string) error {
	f.paused, f.pausedFor = true, d
	return nil
}
func (f *fakeMon) CompleteSetup(context.Context, string) error { f.setup = true; return nil }
func (f *fakeMon) FailoverNow(context.Context, string) error   { f.record("failover"); return nil }
func (f *fakeMon) RestoreNow(context.Context, string) error    { f.record("restore"); return nil }
func (f *fakeMon) CheckNow(context.Context) error              { f.record("check"); return f.failErr }
func (f *fakeMon) Relearn(context.Context, string) error       { f.record("relearn"); return f.failErr }
func (f *fakeMon) PoliciesChanged()                            { f.policies++ }
func (f *fakeMon) LoadBackup(ctx context.Context, replace func(context.Context) error) error {
	if f.loadErr != nil {
		return f.loadErr
	}
	return replace(ctx)
}

type fakeDocker struct{ cs []docker.Container }

func (f fakeDocker) List(context.Context) ([]docker.Container, error) { return f.cs, nil }
func (f fakeDocker) Ping(context.Context) (string, error)             { return "28.0.0", nil }

type noSync struct{}

func (noSync) Sync(context.Context) error { return nil }

func container(project, service string, running bool) docker.Container {
	return docker.Container{ID: "id-" + service, Name: project + "-" + service + "-1", Project: project, Service: service, Running: running}
}

type env struct {
	t      *testing.T
	dbPath string
	srv    *httptest.Server
	ui     *Server
	st     *store.Store
	mon    *fakeMon
}

func (e *env) server() *Server { return e.ui }

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now()
	st.SyncInventory(ctx, []store.InventoryItem{
		{Key: "media/player", Project: "media", Service: "player", Name: "media-player-1"},
		{Key: "media/indexer", Project: "media", Service: "indexer", Name: "media-indexer-1"},
		{Key: "dns", Name: "dns"},
	}, now)
	st.SeeASN(ctx, 64500, "EXAMPLENET-64500", now)
	st.AddEvent(ctx, now, "state_changed", map[string]any{"from": "learning", "to": "primary"})

	mon := &fakeMon{det: config.DefaultDetection()}
	box, _ := secrets.New("test-key")
	ui, err := New(Server{
		Store: st, Mon: mon, Sync: noSync{}, Hub: NewHub(), Secrets: box,
		Docker: fakeDocker{cs: []docker.Container{container("media", "player", true), container("media", "indexer", true),
			{ID: "id-dns", Name: "dns", Running: true}}},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DBPath: "/data/db/drawbridge.db",
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(ui.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, dbPath: dbPath, srv: srv, ui: ui, st: st, mon: mon}
}

// client is a browser: it keeps cookies and does not follow redirects.
type client struct {
	e    *env
	http *http.Client
	csrf string
}

func (e *env) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{e: e, http: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}}
}

func (c *client) do(method, path string, form url.Values, headers ...string) (*http.Response, string) {
	c.e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, c.e.srv.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

var csrfRe = regexp.MustCompile(`name="_csrf" value="([^"]+)"`)

// get loads a page and remembers its CSRF token.
func (c *client) get(path string) (*http.Response, string) {
	resp, body := c.do("GET", path, nil)
	if m := csrfRe.FindStringSubmatch(body); m != nil {
		c.csrf = m[1]
	}
	return resp, body
}

// htmx posts like HTMX does: with the CSRF header.
func (c *client) htmx(path string, form url.Values) (*http.Response, string) {
	return c.do("POST", path, form, "HX-Request", "true", "X-CSRF-Token", c.csrf)
}

func (e *env) admin() *client {
	c := e.client()
	resp, _ := c.do("POST", "/setup/admin", url.Values{
		"username": {"admin"}, "password": {"test-password-123"}, "confirm": {"test-password-123"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		e.t.Fatalf("setup admin: %d", resp.StatusCode)
	}
	c.get("/settings")
	return c
}

func TestSetupRedirectsUntilAdminExists(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	for _, p := range []string{"/", "/login", "/stacks"} {
		if resp, _ := c.do("GET", p, nil); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/setup" {
			t.Errorf("%s: %d %s", p, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	if resp, body := c.get("/setup"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Create the admin account") {
		t.Fatalf("setup page: %d", resp.StatusCode)
	}
	resp, body := c.do("POST", "/setup/admin", url.Values{"username": {"a"}, "password": {"short"}, "confirm": {"short"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "at least 12") {
		t.Fatalf("short password: %d", resp.StatusCode)
	}
	e.admin()
	// Once an admin exists, step 1 cannot be replayed.
	if resp, _ := e.client().do("POST", "/setup/admin", url.Values{
		"username": {"evil"}, "password": {"test-password-123"}, "confirm": {"test-password-123"},
	}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("second admin via setup: %d", resp.StatusCode)
	}
}

func TestWizardSteps(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	for step, want := range map[string]string{"2": "Single sign-on (optional)", "3": "Connected to Docker 28.0.0",
		"4": "WAN detection", "5": "Primary ISP", "6": "What to stop", "7": "Review and start"} {
		if resp, body := c.get("/setup?step=" + step); resp.StatusCode != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("step %s: %d, missing %q", step, resp.StatusCode, want)
		}
	}
	isp, _ := e.st.ISPOfASN(context.Background(), 64500)
	if resp, _ := c.htmx("/setup/primary", url.Values{"choice": {"pin"}, "isp": {"1"}}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("pin: %d", resp.StatusCode)
	}
	if id, pinned, _ := e.st.Primary(context.Background()); id != isp || !pinned {
		t.Fatalf("primary = %d pinned=%v", id, pinned)
	}
	if resp, _ := c.htmx("/setup/finish", url.Values{}); resp.StatusCode != http.StatusNoContent || !e.mon.setup {
		t.Fatalf("finish: %d", resp.StatusCode)
	}
}

func TestEveryPageRenders(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	e.st.SetSetting(context.Background(), store.SettingSetupComplete, true)
	pages := map[string]string{
		"/":                         "Dashboard",
		"/partials/status":          "On primary",
		"/partials/timeline":        "Last 24 hours",
		"/stacks":                   "media",
		"/stacks/media/containers":  "media-player-1",
		"/containers":               "media-indexer-1",
		"/containers?stack=-":       "dns",
		"/containers?policy=review": "No containers match",
		"/isps":                     "EXAMPLENET",
		"/events":                   "state_changed",
		"/settings":                 "Check interval",
		"/users":                    "admin",
	}
	for path, want := range pages {
		resp, body := c.get(path)
		if path == "/partials/timeline" {
			want = "timeline"
		}
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("%s: %d, missing %q\n%s", path, resp.StatusCode, want, body[:min(len(body), 300)])
		}
	}
	resp, body := c.get("/events.csv")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(body, "time,type,actor,details") {
		t.Errorf("csv: %d %q", resp.StatusCode, body)
	}
}

func TestLoginAndRateLimit(t *testing.T) {
	e := newEnv(t)
	e.admin()
	c := e.client()
	if resp, _ := c.do("GET", "/stacks", nil); resp.StatusCode != http.StatusSeeOther ||
		!strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Fatalf("anonymous: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	for i := range 5 {
		if resp, _ := c.do("POST", "/login", url.Values{"username": {"admin"}, "password": {"wrong password!!"}}); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, resp.StatusCode)
		}
	}
	// Even the right password is refused once the limit is hit.
	if resp, _ := c.do("POST", "/login", url.Values{"username": {"admin"}, "password": {"test-password-123"}}); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("after 5 failures: %d", resp.StatusCode)
	}
}

func TestLoginRedirectStaysLocal(t *testing.T) {
	e := newEnv(t)
	e.admin()
	c := e.client()
	resp, _ := c.do("POST", "/login", url.Values{
		"username": {"admin"}, "password": {"test-password-123"}, "next": {"//evil.example/"},
	})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("next: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestCSRFAndCrossOrigin(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	if resp, _ := c.do("POST", "/stacks/media/policy", url.Values{"disable": {"on"}}, "HX-Request", "true"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	if resp, _ := c.do("POST", "/stacks/media/policy", url.Values{"disable": {"on"}},
		"X-CSRF-Token", c.csrf, "Sec-Fetch-Site", "cross-site"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site: %d", resp.StatusCode)
	}
	if resp, _ := c.htmx("/stacks/media/policy", url.Values{"disable": {"on"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("with token: %d", resp.StatusCode)
	}
}

func TestViewerIsReadOnly(t *testing.T) {
	e := newEnv(t)
	a := e.admin()
	if resp, _ := a.htmx("/users", url.Values{"username": {"viewer"}, "password": {"test-password-123"}, "role": {"viewer"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("create viewer: %d", resp.StatusCode)
	}
	v := e.client()
	if resp, _ := v.do("POST", "/login", url.Values{"username": {"viewer"}, "password": {"test-password-123"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("viewer login: %d", resp.StatusCode)
	}
	resp, body := v.get("/containers")
	if resp.StatusCode != http.StatusOK || strings.Contains(body, `hx-post="/containers/policy"`) {
		t.Fatalf("viewer sees policy controls: %d", resp.StatusCode)
	}
	for _, p := range []string{"/stacks/media/policy", "/actions/failover", "/settings/detection", "/users"} {
		if resp, _ := v.htmx(p, url.Values{}); resp.StatusCode != http.StatusForbidden {
			t.Errorf("viewer POST %s: %d", p, resp.StatusCode)
		}
	}
	if resp, _ := v.get("/users"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("viewer GET /users: %d", resp.StatusCode)
	}
}

func TestContainerOverrideConvertsStack(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	c.htmx("/stacks/media/policy", url.Values{"disable": {"on"}})
	_, body := c.get("/containers")
	if !strings.Contains(body, "This removes the stack-wide setting from media. The other 1 containers") {
		t.Fatal("conversion confirmation missing")
	}
	resp, body := c.htmx("/containers/policy", url.Values{"key": {"media/player"}, "policy": {"keep"}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "own setting") {
		t.Fatalf("override: %d", resp.StatusCode)
	}
	rows, _ := e.st.Containers(context.Background())
	for _, r := range rows {
		want := map[string]string{"media/player": "keep", "media/indexer": "disable", "dns": "keep"}[r.Key]
		if r.Effective != want {
			t.Errorf("%s = %s, want %s", r.Key, r.Effective, want)
		}
	}
	if e.mon.policies != 2 {
		t.Errorf("monitor told about %d policy changes, want 2", e.mon.policies)
	}
}

func TestDetectionSettingsValidate(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	form := url.Values{"check_interval": {"5s"}, "failover_after": {"45s"}, "failback_after": {"5m"},
		"failback_max_hold": {"2h"}, "quorum": {"2"},
		"timeout": {"5s"}, "learning_period": {"1d"}, "retention": {"30d"},
		"ip_providers": {"https://a.example\nhttps://b.example"}}
	resp, body := c.htmx("/settings/detection", form)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "check interval") {
		t.Fatalf("bad interval: %d %s", resp.StatusCode, body)
	}
	form.Set("check_interval", "2m")
	if resp, _ := c.htmx("/settings/detection", form); resp.StatusCode != http.StatusOK {
		t.Fatalf("good settings: %d", resp.StatusCode)
	}
	if e.mon.det.CheckInterval != 2*time.Minute || e.mon.det.LearningPeriod != 24*time.Hour || len(e.mon.det.IPProviders) != 2 {
		t.Fatalf("applied %+v", e.mon.det)
	}
}

func TestActions(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	for _, a := range []string{"failover", "restore", "check"} {
		if resp, _ := c.htmx("/actions/"+a, url.Values{}); resp.StatusCode != http.StatusOK {
			t.Errorf("%s: %d", a, resp.StatusCode)
		}
	}
	if strings.Join(e.mon.calls, ",") != "failover,restore,check" {
		t.Errorf("calls = %v", e.mon.calls)
	}
	c.htmx("/actions/pause", url.Values{})
	if !e.mon.paused || e.mon.pausedFor != 0 {
		t.Error("not paused until resumed")
	}
	resp, body := c.htmx("/actions/pause", url.Values{"for": {"4h"}})
	if resp.StatusCode != http.StatusOK || e.mon.pausedFor != 4*time.Hour || !strings.Contains(body, "Paused until ") {
		t.Errorf("pause for 4h: %d %v", resp.StatusCode, e.mon.pausedFor)
	}
	e.st.SetSetting(context.Background(), store.SettingSetupComplete, true)
	if _, page := c.get("/"); !strings.Contains(page, "Automation is paused until ") {
		t.Error("banner does not say when the pause ends")
	}
	if resp, _ := c.htmx("/actions/pause", url.Values{"for": {"7d"}}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown duration: %d", resp.StatusCode)
	}
}

func TestClock(t *testing.T) {
	now := time.Now()
	if got := clock(now); got != now.Format("15:04") {
		t.Errorf("today: %q", got)
	}
	if got := clock(now.Add(48 * time.Hour)); got != now.Add(48*time.Hour).Format("Mon 15:04") {
		t.Errorf("later: %q", got)
	}
}

func TestRestoreOfferedWhileContainersAreStopped(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	if _, body := c.get("/partials/status"); !strings.Contains(body, "Fail over now") {
		t.Fatal("no open run: expected Fail over now")
	}
	ctx := context.Background()
	run, _ := e.st.CreateRun(ctx, "manual", 0, time.Now())
	e.st.AddRunAction(ctx, run.ID, store.RunAction{ContainerID: "id-player", Key: "media/player", Name: "media-player-1", WasRunning: true})
	if _, body := c.get("/partials/status"); !strings.Contains(body, "Restore now") || strings.Contains(body, "Fail over now") {
		t.Fatal("open run: expected Restore now only")
	}
}

func TestMetrics(t *testing.T) {
	e := newEnv(t)
	e.admin()
	c := e.client()
	if resp, _ := c.do("GET", "/metrics", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("metrics without a token configured: %d", resp.StatusCode)
	}
	e.ui.MetricsToken = "scrape-me"
	if resp, _ := c.do("GET", "/metrics", nil, "Authorization", "Bearer wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	resp, body := c.do("GET", "/metrics", nil, "Authorization", "Bearer scrape-me")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics: %d", resp.StatusCode)
	}
	for _, want := range []string{
		`drawbridge_wan_state{state="primary"} 1`,
		`drawbridge_wan_state{state="failover"} 0`,
		`drawbridge_on_primary 1`,
		`drawbridge_isp_info{isp="EXAMPLENET",asn="64500",primary="true"} 1`,
		`drawbridge_containers{on_failover="keep"} 3`,
		`# TYPE drawbridge_checks_total counter`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}

func TestFooterLinksSource(t *testing.T) {
	e := newEnv(t)
	e.ui.SourceURL = "https://gitlab.example/me/wan-drawbridge"
	e.admin()
	_, body := e.client().get("/login")
	if !strings.Contains(body, `href="https://gitlab.example/me/wan-drawbridge"`) || !strings.Contains(body, "AGPL v3 or later") {
		t.Fatal("login page lacks the license and source link")
	}
}

func TestDetectionValidatesFailbackWait(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	form := url.Values{"check_interval": {"1m"}, "failover_after": {"45s"}, "failback_after": {"30m"},
		"failback_max_hold": {"10m"}, "quorum": {"2"}, "timeout": {"5s"}, "learning_period": {"1d"},
		"retention": {"30d"}, "ip_providers": {"https://a.example\nhttps://b.example"}}
	if resp, body := c.htmx("/settings/detection", form); resp.StatusCode != http.StatusUnprocessableEntity ||
		!strings.Contains(body, "cannot be shorter than the failback delay") {
		t.Fatalf("max hold below failback delay: %d %s", resp.StatusCode, body)
	}
}

func TestDashboardShowsFlapping(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	e.mon.flaps = 2
	_, body := c.get("/partials/status")
	if !strings.Contains(body, "has been flapping") || !strings.Contains(body, "waits 20m") {
		t.Fatalf("flapping notice missing:\n%s", body)
	}
}
