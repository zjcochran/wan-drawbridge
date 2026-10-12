// SPDX-License-Identifier: AGPL-3.0-or-later

// Package e2e runs Drawbridge end to end in one process: the real store,
// monitor, executor, Docker client and web server, against a fake Docker
// Engine API and fake public-IP and ASN services.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/asn"
	"github.com/zjcochran/wan-drawbridge/internal/config"
	"github.com/zjcochran/wan-drawbridge/internal/docker"
	"github.com/zjcochran/wan-drawbridge/internal/executor"
	"github.com/zjcochran/wan-drawbridge/internal/monitor"
	"github.com/zjcochran/wan-drawbridge/internal/secrets"
	"github.com/zjcochran/wan-drawbridge/internal/store"
	"github.com/zjcochran/wan-drawbridge/internal/wan"
	"github.com/zjcochran/wan-drawbridge/internal/web"
)

// ---- fake Docker Engine API ----

type fakeContainer struct {
	ID, Name, Project, Service, DependsOn string
	Running                               bool
}

type fakeDocker struct {
	mu  sync.Mutex
	cs  []*fakeContainer
	ops []string
	srv *httptest.Server
}

func newFakeDocker(t *testing.T) *fakeDocker {
	f := &fakeDocker{}
	add := func(project, service string, running bool, deps string) {
		name := service
		if project != "" {
			name = project + "-" + service + "-1"
		}
		f.cs = append(f.cs, &fakeContainer{ID: fmt.Sprintf("%064x", len(f.cs)+1), Name: name,
			Project: project, Service: service, Running: running, DependsOn: deps})
	}
	add("media", "db", true, "")
	add("media", "player", true, "db:service_started:false")
	add("media", "indexer", true, "")
	add("media", "tdarr", false, "") // already stopped: must stay stopped
	add("", "dns", true, "")         // bystander

	idRe := regexp.MustCompile(`/containers/([0-9a-f]+)/(json|stop|start)$`)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/version":
			json.NewEncoder(w).Encode(map[string]string{"Version": "28.0.0-e2e"})
		case "/events":
			f.mu.Unlock()
			<-r.Context().Done()
			f.mu.Lock()
		case "/containers/json":
			var out []map[string]any
			for _, c := range f.cs {
				labels := map[string]string{}
				if c.Project != "" {
					labels[docker.LabelProject], labels[docker.LabelService] = c.Project, c.Service
				}
				if c.DependsOn != "" {
					labels[docker.LabelDependsOn] = c.DependsOn
				}
				state := "exited"
				if c.Running {
					state = "running"
				}
				out = append(out, map[string]any{"Id": c.ID, "Names": []string{"/" + c.Name}, "State": state, "Labels": labels})
			}
			json.NewEncoder(w).Encode(out)
		default:
			m := idRe.FindStringSubmatch(r.URL.Path)
			var c *fakeContainer
			for _, x := range f.cs {
				if m != nil && x.ID == m[1] {
					c = x
				}
			}
			if c == nil {
				http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
				return
			}
			switch m[2] {
			case "json":
				json.NewEncoder(w).Encode(map[string]any{"State": map[string]any{"Running": c.Running}})
			case "stop", "start":
				c.Running = m[2] == "start"
				f.ops = append(f.ops, m[2]+" "+c.Name)
				w.WriteHeader(http.StatusNoContent)
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
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

// ---- fake WAN: public IP and ASN lookup ----

type fakeWAN struct {
	ip  atomic.Value
	srv *httptest.Server
}

func newFakeWAN(t *testing.T) *fakeWAN {
	f := &fakeWAN{}
	f.ip.Store("203.0.113.3")
	orgs := map[string]string{"203.0.113.3": "AS64500 EXAMPLENET-64500", "203.0.113.9": "AS64501 EXAMPLENET-64501",
		"198.51.100.3": "AS64510 SKYLINK-AS64510"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ip, ok := strings.CutPrefix(r.URL.Path, "/asn/"); ok {
			fmt.Fprintf(w, `{"org":%q}`, orgs[ip])
			return
		}
		if r.URL.Path == "/ip" {
			io.WriteString(w, f.ip.Load().(string))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// ---- the system under test ----

type system struct {
	cancel context.CancelFunc
	done   chan struct{}
	mon    *monitor.Monitor
	web    *httptest.Server
}

func start(t *testing.T, dbDir string, d *fakeDocker, w *fakeWAN) *system {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(ctx, filepath.Join(dbDir, "drawbridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	dc, err := docker.New("tcp://" + strings.TrimPrefix(d.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.Load("", filepath.Join(dbDir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	res := asn.NewResolver(dbDir, log)
	res.DBURL, res.OnlineURL = w.srv.URL+"/no-db", w.srv.URL+"/asn/%s"

	cfg := config.Config{Detection: config.DefaultDetection(), PrimaryASN: 64500}
	// Far below the UI's minimums, to keep the test fast.
	cfg.CheckInterval = 100 * time.Millisecond
	cfg.FailoverAfter, cfg.FailbackAfter, cfg.FailbackMaxHold = 300*time.Millisecond, 700*time.Millisecond, 5*time.Second
	cfg.IPProviders = []string{w.srv.URL + "/ip", w.srv.URL + "/ip?second"}

	hub := web.NewHub()
	ex := executor.New(dc, st, log, "")
	ex.PollEvery = 10 * time.Millisecond
	ex.OnChange = func() { hub.Publish(web.TopicInventory) }
	mon := monitor.New(cfg, st, res, ex, log, func() { hub.Publish(web.TopicState) })
	ui, err := web.New(web.Server{Store: st, Mon: mon, Docker: dc, Sync: ex, Hub: hub, Log: log,
		Secrets: box, MetricsToken: "e2e", DBPath: filepath.Join(dbDir, "drawbridge.db")})
	if err != nil {
		t.Fatal(err)
	}
	s := &system{cancel: cancel, done: make(chan struct{}), mon: mon, web: httptest.NewServer(ui.Handler())}
	go ex.WatchInventory(ctx, dc, time.Second)
	go func() { mon.Run(ctx); close(s.done) }()
	return s
}

// stop simulates the process dying.
func (s *system) stop() {
	s.cancel()
	<-s.done
	s.web.Close()
}

type browser struct {
	t    *testing.T
	base string
	http *http.Client
	csrf string
}

func newBrowser(t *testing.T, base string) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, base: base, http: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}}
}

func (b *browser) post(path string, form url.Values) int {
	b.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, b.base+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("X-CSRF-Token", b.csrf)
	resp, err := b.http.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (b *browser) get(path string, headers ...string) string {
	b.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, b.base+path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := b.http.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if m := regexp.MustCompile(`name="_csrf" value="([^"]+)"`).FindSubmatch(body); m != nil {
		b.csrf = string(m[1])
	}
	return string(body)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestFailoverFailbackAndCrashRecovery(t *testing.T) {
	dbDir := t.TempDir()
	dock, wanSim := newFakeDocker(t), newFakeWAN(t)
	sys := start(t, dbDir, dock, wanSim)

	// Setup through the web UI, like a person would.
	b := newBrowser(t, sys.web.URL)
	req, _ := http.NewRequest(http.MethodPost, sys.web.URL+"/setup/admin", strings.NewReader(url.Values{
		"username": {"admin"}, "password": {"test-password-123"}, "confirm": {"test-password-123"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if resp, err := b.http.Do(req); err != nil || resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create admin: %v %v", resp, err)
	}
	b.get("/settings")
	waitFor(t, "inventory", func() bool { body := b.get("/stacks"); return strings.Contains(body, "media") })
	if code := b.post("/stacks/media/policy", url.Values{"disable": {"on"}}); code != 200 {
		t.Fatalf("stack policy: %d", code)
	}
	if code := b.post("/setup/finish", url.Values{}); code != http.StatusNoContent {
		t.Fatalf("finish setup: %d", code)
	}
	waitFor(t, "primary", func() bool { return sys.mon.Status().State == wan.Primary })

	// A reconnect onto another Examplenet ASN is the same ISP: no failover.
	wanSim.ip.Store("203.0.113.9")
	time.Sleep(time.Second) // five checks or so
	if s := sys.mon.Status().State; s != wan.Primary {
		t.Fatalf("other Examplenet ASN changed state to %s", s)
	}

	// Traffic moves to Skylink: the media stack stops, the rest is untouched.
	wanSim.ip.Store("198.51.100.3")
	waitFor(t, "failover", func() bool { return !dock.running("media-db-1") && !dock.running("media-player-1") })
	if !dock.running("dns") {
		t.Fatal("bystander was stopped")
	}
	// The third container may still be stopping: wait for the run to record it.
	waitFor(t, "failover metrics", func() bool {
		metrics := b.get("/metrics", "Authorization", "Bearer e2e")
		return strings.Contains(metrics, `drawbridge_wan_state{state="failover"} 1`) &&
			strings.Contains(metrics, "drawbridge_containers_stopped 3")
	})

	// Drawbridge dies while on the backup and comes back.
	sys.stop()
	sys = start(t, dbDir, dock, wanSim)
	defer sys.stop()
	b.base = sys.web.URL // the session cookie survives: sessions live in the database
	waitFor(t, "state reload", func() bool { return sys.mon.Status().State != "" && !sys.mon.Status().LastCheck.IsZero() })
	if s := sys.mon.Status().State; s != wan.Failover && s != wan.FailbackPending {
		t.Fatalf("after restart: %s", s)
	}

	// The primary returns: what Drawbridge stopped starts again, dependencies first.
	wanSim.ip.Store("203.0.113.3")
	waitFor(t, "failback", func() bool {
		return dock.running("media-db-1") && dock.running("media-player-1") && dock.running("media-indexer-1")
	})
	if dock.running("media-tdarr-1") {
		t.Fatal("a container that was already stopped was started")
	}
	dock.mu.Lock()
	ops := strings.Join(dock.ops, ", ")
	dock.mu.Unlock()
	if strings.Index(ops, "stop media-player-1") > strings.Index(ops, "stop media-db-1") ||
		strings.LastIndex(ops, "start media-db-1") > strings.LastIndex(ops, "start media-player-1") {
		t.Fatalf("dependency order wrong: %s", ops)
	}
	waitFor(t, "run closed", func() bool {
		m := b.get("/metrics", "Authorization", "Bearer e2e")
		return strings.Contains(m, "drawbridge_containers_stopped 0")
	})
	t.Logf("docker operations: %s", ops)
}
