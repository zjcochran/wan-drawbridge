// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/asn"
	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// fakeDocker answers the few Engine API calls Drawbridge makes.
func fakeDocker(t *testing.T, broken bool) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if broken {
			http.Error(w, `{"message":"daemon down"}`, http.StatusInternalServerError)
			return
		}
		switch {
		case r.URL.Path == "/version":
			fmt.Fprint(w, `{"Version":"28.5.2"}`)
		case r.URL.Path == "/containers/json":
			fmt.Fprint(w, `[
				{"Id":"p1","Names":["/media-player-1"],"State":"running","Labels":{"com.docker.compose.project":"media","com.docker.compose.service":"player"}},
				{"Id":"s1","Names":["/media-indexer-1"],"State":"running","Labels":{"com.docker.compose.project":"media","com.docker.compose.service":"indexer"}},
				{"Id":"h1","Names":["/dns"],"State":"running","Labels":{}}]`)
		case r.URL.Path == "/events":
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case strings.HasSuffix(r.URL.Path, "/json"):
			fmt.Fprint(w, `{"State":{"Running":true}}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeWAN serves the public IP and the ASN database.
func fakeWAN(t *testing.T) *httptest.Server {
	var db bytes.Buffer
	gz := gzip.NewWriter(&db)
	fmt.Fprint(gz, "203.0.113.0\t203.0.113.255\t64500\tUS\tEXAMPLENET-64500\n")
	gz.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ip":
			fmt.Fprint(w, "203.0.113.3")
		case "/db.tsv.gz":
			w.Write(db.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// setup points Drawbridge at fakes and fresh directories, and returns the DB path.
func setup(t *testing.T, brokenDocker bool) string {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "db"), 0o750)
	wanSrv := fakeWAN(t)
	t.Setenv("DRAWBRIDGE_DB_DIR", filepath.Join(dir, "db"))
	t.Setenv("DRAWBRIDGE_CACHE_DIR", filepath.Join(dir, "cache"))
	t.Setenv("DOCKER_HOST", fakeDocker(t, brokenDocker).URL)
	t.Setenv("DRAWBRIDGE_IP_PROVIDERS", wanSrv.URL+"/ip")
	t.Setenv("DRAWBRIDGE_QUORUM", "1")
	t.Setenv("LOG_LEVEL", "error")
	orig := newResolver
	newResolver = func(cacheDir string, log *slog.Logger) *asn.Resolver {
		r := orig(cacheDir, log)
		r.DBURL, r.OnlineURL = wanSrv.URL+"/db.tsv.gz", ""
		return r
	}
	t.Cleanup(func() { newResolver = orig })
	return filepath.Join(dir, "db", "drawbridge.db")
}

// drawbridge runs one CLI invocation in-process.
func drawbridge(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = cli(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func withStore(t *testing.T, path string, fn func(context.Context, *store.Store)) {
	t.Helper()
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fn(context.Background(), st)
}

func TestVersion(t *testing.T) {
	if code, out, _ := drawbridge(t, "", "version"); code != 0 || out != "dev\n" {
		t.Fatalf("version: %d %q", code, out)
	}
}

func TestUsageOnBadArguments(t *testing.T) {
	setup(t, false)
	for _, args := range [][]string{{"nope"}, {"set-stack", "media", "maybe"}, {"set-isp", "x", "primary"},
		{"move-asn", "ASx", "1"}, {"status", "extra"}} {
		if code, _, errOut := drawbridge(t, "", args...); code != 1 || !strings.Contains(errOut, "usage:") {
			t.Errorf("%v: %d %q", args, code, errOut)
		}
	}
}

func TestInvalidConfiguration(t *testing.T) {
	setup(t, false)
	t.Setenv("DRAWBRIDGE_QUORUM", "two")
	if code, _, errOut := drawbridge(t, "", "status"); code != 1 || !strings.Contains(errOut, "DRAWBRIDGE_QUORUM") {
		t.Fatalf("bad config: %d %q", code, errOut)
	}
}

func TestStacksAndContainers(t *testing.T) {
	setup(t, false)
	code, out, _ := drawbridge(t, "", "stacks")
	if code != 0 || !strings.Contains(out, "media") || !strings.Contains(out, "none") {
		t.Fatalf("stacks: %d %q", code, out)
	}
	if code, out, _ := drawbridge(t, "", "set-stack", "media", "on"); code != 0 || !strings.Contains(out, "disable on failover on") {
		t.Fatalf("set-stack: %d %q", code, out)
	}
	if _, out, _ := drawbridge(t, "", "containers"); !strings.Contains(out, "from stack") || !strings.Contains(out, "dns") {
		t.Fatalf("containers after set-stack: %q", out)
	}
	code, out, _ = drawbridge(t, "", "set-container", "media/player", "keep")
	if code != 0 || !strings.Contains(out, "stack-wide setting was removed") {
		t.Fatalf("set-container: %d %q", code, out)
	}
	if _, out, _ := drawbridge(t, "", "stacks"); !strings.Contains(out, "partial") {
		t.Fatalf("stack after an override: %q", out)
	}
	if code, _, errOut := drawbridge(t, "", "set-container", "media/player", "sometimes"); code != 1 || errOut == "" {
		t.Fatalf("bad policy: %d %q", code, errOut)
	}
	if code, _, _ := drawbridge(t, "", "set-stack", "nope", "on"); code != 1 {
		t.Fatalf("unknown stack: %d", code)
	}
}

func TestStatusShowsOpenRun(t *testing.T) {
	db := setup(t, false)
	if code, out, _ := drawbridge(t, "", "status"); code != 0 || !strings.Contains(out, "learning") || !strings.Contains(out, "none") {
		t.Fatalf("status: %d %q", code, out)
	}
	withStore(t, db, func(ctx context.Context, st *store.Store) {
		run, _ := st.CreateRun(ctx, "auto", 64510, time.Now())
		st.AddRunAction(ctx, run.ID, store.RunAction{ContainerID: "p1", Key: "media/player", Name: "media-player-1", WasRunning: true})
		st.MarkStopped(ctx, run.ID, "p1", "", time.Now())
	})
	if _, out, _ := drawbridge(t, "", "status"); !strings.Contains(out, "open run") || !strings.Contains(out, "media-player-1") {
		t.Fatalf("status with a run: %q", out)
	}
}

func TestISPCommands(t *testing.T) {
	db := setup(t, false)
	withStore(t, db, func(ctx context.Context, st *store.Store) {
		st.SeeASN(ctx, 64500, "EXAMPLENET-64500", time.Now())
		st.SeeASN(ctx, 64510, "SKYLINK-AS64510", time.Now())
	})
	if code, out, _ := drawbridge(t, "", "set-isp", "1", "primary"); code != 0 || !strings.Contains(out, "ISP 1 is now primary") {
		t.Fatalf("set-isp: %d %q", code, out)
	}
	if code, out, _ := drawbridge(t, "", "move-asn", "AS64510", "1"); code != 0 || !strings.Contains(out, "AS64510 is now in ISP 1") {
		t.Fatalf("move-asn: %d %q", code, out)
	}
	code, out, _ := drawbridge(t, "", "isps")
	if code != 0 || !strings.Contains(out, "primary (pinned)") || !strings.Contains(out, "AS64510 SKYLINK-AS64510 (manual)") {
		t.Fatalf("isps: %d %q", code, out)
	}
	if code, out, _ := drawbridge(t, "", "move-asn", "64510", "new"); code != 0 || strings.Contains(out, "ISP 1\n") {
		t.Fatalf("move-asn new: %d %q", code, out)
	}
	_, out, _ = drawbridge(t, "", "isps")
	id := strings.Fields(strings.Split(out, "\n")[2])[0] // the new ISP of AS64510
	if code, out, _ := drawbridge(t, "", "set-isp", id, "unmetered"); code != 0 || !strings.Contains(out, "is now unmetered") {
		t.Fatalf("set-isp unmetered: %d %q", code, out)
	}
	if _, out, _ := drawbridge(t, "", "isps"); !strings.Contains(out, "backup (unmetered)") {
		t.Fatalf("isps after unmetered: %q", out)
	}
	if code, _, _ := drawbridge(t, "", "set-isp", "99", "metered"); code != 1 {
		t.Fatal("an unknown ISP was marked metered")
	}
	if code, _, _ := drawbridge(t, "", "set-isp", "1", "owner"); code != 1 {
		t.Fatal("bad role accepted")
	}
	if code, _, _ := drawbridge(t, "", "move-asn", "64512", "1"); code != 1 {
		t.Fatal("moving an unseen ASN should fail")
	}
}

func TestResetPassword(t *testing.T) {
	db := setup(t, false)
	withStore(t, db, func(ctx context.Context, st *store.Store) {
		u, _ := st.CreateUser(ctx, "Admin", "test-password-123", store.RoleAdmin, time.Now())
		st.CreateUser(ctx, "amy", "test-password-123", store.RoleAdmin, time.Now())
		st.SetUserDisabled(ctx, u.ID, true)
	})
	code, out, errOut := drawbridge(t, "a brand new password\n", "reset-password", "admin")
	if code != 0 || !strings.Contains(out, "password for Admin changed") || !strings.Contains(errOut, "new password") {
		t.Fatalf("reset: %d %q %q", code, out, errOut)
	}
	withStore(t, db, func(ctx context.Context, st *store.Store) {
		if _, err := st.Authenticate(ctx, "Admin", "a brand new password", time.Now()); err != nil {
			t.Fatalf("new password, and re-enabled: %v", err)
		}
	})
	if code, _, _ := drawbridge(t, "short\n", "reset-password", "admin"); code != 1 {
		t.Fatal("short password accepted")
	}
	if code, _, _ := drawbridge(t, "", "reset-password", "admin"); code != 1 {
		t.Fatal("empty input accepted")
	}
	if code, _, errOut := drawbridge(t, "x\n", "reset-password", "nobody"); code != 1 || !strings.Contains(errOut, `no user "nobody"`) {
		t.Fatalf("unknown user: %d %q", code, errOut)
	}
}

func TestDockerDownShowsStoredInventory(t *testing.T) {
	setup(t, true)
	code, out, errOut := drawbridge(t, "", "stacks")
	if code != 0 || !strings.Contains(errOut, "could not read Docker") || !strings.Contains(out, "STACK") {
		t.Fatalf("stacks without Docker: %d %q %q", code, out, errOut)
	}
}

func TestStoreErrors(t *testing.T) {
	setup(t, false)
	t.Setenv("DRAWBRIDGE_DB_DIR", "/dev/null/db") // cannot be created
	if code, _, _ := drawbridge(t, "", "status"); code != 1 {
		t.Fatal("expected an error for an unusable database directory")
	}
}

func TestBadDockerHost(t *testing.T) {
	setup(t, false)
	t.Setenv("DOCKER_HOST", "ssh://somewhere")
	if code, _, errOut := drawbridge(t, "", "status"); code != 1 || !strings.Contains(errOut, "unsupported scheme") {
		t.Fatalf("bad DOCKER_HOST: %d %q", code, errOut)
	}
}

func TestHealthcheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
		}
	}))
	defer ok.Close()
	t.Setenv("DRAWBRIDGE_LISTEN", ok.Listener.Addr().String())
	if code, _, errOut := drawbridge(t, "", "healthcheck"); code != 0 {
		t.Fatalf("healthy: %d %q", code, errOut)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	t.Setenv("DRAWBRIDGE_LISTEN", bad.Listener.Addr().String())
	if code, _, errOut := drawbridge(t, "", "healthcheck"); code != 1 || !strings.Contains(errOut, "status 503") {
		t.Fatalf("unhealthy: %d %q", code, errOut)
	}

	t.Setenv("DRAWBRIDGE_LISTEN", "no port")
	if code, _, _ := drawbridge(t, "", "healthcheck"); code != 1 {
		t.Fatal("a bad listen address should fail")
	}
	// ":port" means all interfaces; the check goes to loopback.
	_, port, _ := net.SplitHostPort(ok.Listener.Addr().String())
	t.Setenv("DRAWBRIDGE_LISTEN", ":"+port)
	if code, _, errOut := drawbridge(t, "", "healthcheck"); code != 0 {
		t.Fatalf("all interfaces: %d %q", code, errOut)
	}
}

func freePort(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func TestServeStartsAndStops(t *testing.T) {
	setup(t, false)
	addr := freePort(t)
	t.Setenv("DRAWBRIDGE_LISTEN", addr)
	t.Setenv("DRAWBRIDGE_LOCAL_LOGIN", "force")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var stderr bytes.Buffer
	go func() { done <- run(ctx, nil, strings.NewReader(""), &bytes.Buffer{}, &stderr) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("server never became healthy: %v\n%s", err, stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	resp, err := http.Get("http://" + addr + "/login")
	if err != nil || resp.Request.URL.Path != "/setup" {
		t.Fatalf("first visit should go to setup: %v %v", resp, err)
	}
	resp.Body.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
}

func TestServeSendsLogToSyslog(t *testing.T) {
	setup(t, false)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	t.Setenv("DRAWBRIDGE_SYSLOG", "udp://"+pc.LocalAddr().String())
	t.Setenv("DRAWBRIDGE_LISTEN", freePort(t))
	t.Setenv("LOG_LEVEL", "info")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var stderr bytes.Buffer
	go func() { done <- run(ctx, nil, strings.NewReader(""), &bytes.Buffer{}, &stderr) }()

	buf := make([]byte, 4096)
	pc.SetReadDeadline(time.Now().Add(10 * time.Second))
	var got []string
	for !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, `msg="web UI listening"`) }) {
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			cancel()
			t.Fatalf("no syslog message about the web UI: %v\n%q", err, got)
		}
		got = append(got, string(buf[:n]))
	}
	if !strings.HasPrefix(got[0], "<30>1 ") || !strings.Contains(got[0], `msg="sending the log to syslog"`) {
		t.Errorf("first message: %q", got[0])
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "web UI listening") {
		t.Error("the log no longer reaches stderr")
	}
}

func TestServeReportsStartupErrors(t *testing.T) {
	setup(t, true) // Docker down only logs an error; startup continues
	t.Setenv("DRAWBRIDGE_TRUSTED_PROXIES", "not-an-ip")
	if err := run(context.Background(), nil, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("bad trusted proxies should stop startup")
	}
	t.Setenv("DRAWBRIDGE_TRUSTED_PROXIES", "")
	t.Setenv("DRAWBRIDGE_LISTEN", "127.0.0.1:-1")
	if err := run(context.Background(), nil, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("a bad listen address should stop the server")
	}
}
