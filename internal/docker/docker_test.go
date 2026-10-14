// SPDX-License-Identifier: AGPL-3.0-or-later

package docker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeEngine answers the Engine API calls TestClient makes, recording the
// stop and start requests in posted.
func fakeEngine(posted *[]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/json":
			fmt.Fprint(w, `[
				{"Id":"aaa111","Names":["/media-player-1"],"State":"running","Labels":{
					"com.docker.compose.project":"media","com.docker.compose.service":"player",
					"com.docker.compose.depends_on":"db:service_healthy:false, redis:service_started:true"}},
				{"Id":"bbb222","Names":["/standalone"],"State":"exited","Labels":{}}]`)
		case r.URL.Path == "/containers/aaa111/stats":
			fmt.Fprint(w, `{"networks":{"eth0":{"rx_bytes":1000,"tx_bytes":200},"eth1":{"rx_bytes":5,"tx_bytes":7}}}`)
		case r.URL.Path == "/containers/bbb222/stats":
			fmt.Fprint(w, `{"read":"0001-01-01T00:00:00Z"}`)
		case r.URL.Path == "/containers/aaa111/json":
			fmt.Fprint(w, `{"State":{"Running":true,"Health":{"Status":"healthy"}}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
			*posted = append(*posted, r.URL.Path+"?"+r.URL.RawQuery)
			w.WriteHeader(http.StatusNotModified) // already stopped
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			if strings.Contains(r.URL.Path, "missing") {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"message":"No such container: missing"}`)
				return
			}
			*posted = append(*posted, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
}

func TestClient(t *testing.T) {
	var posted []string
	srv := httptest.NewServer(fakeEngine(&posted))
	defer srv.Close()

	c, err := New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	cs, err := c.List(ctx)
	if err != nil || len(cs) != 2 {
		t.Fatalf("List = %+v, %v", cs, err)
	}
	player, solo := cs[0], cs[1]
	if player.Key() != "media/player" || !player.Running || !slices.Equal(player.DependsOn(), []string{"db", "redis"}) {
		t.Errorf("player = %+v deps=%v", player, player.DependsOn())
	}
	if solo.Key() != "standalone" || solo.Running || solo.DependsOn() != nil {
		t.Errorf("standalone = %+v", solo)
	}

	running, health, err := c.Inspect(ctx, "aaa111")
	if err != nil || !running || health != "healthy" {
		t.Errorf("Inspect = %v %q %v", running, health, err)
	}
	if err := c.Stop(ctx, "aaa111", 30*time.Second); err != nil {
		t.Errorf("Stop on already stopped container: %v", err)
	}
	if err := c.Start(ctx, "aaa111"); err != nil {
		t.Errorf("Start: %v", err)
	}
	if err := c.Start(ctx, "missing"); err == nil || !strings.Contains(err.Error(), "No such container") {
		t.Errorf("Start(missing) = %v", err)
	}
	want := []string{"/containers/aaa111/stop?t=30", "/containers/aaa111/start"}
	if !slices.Equal(posted, want) {
		t.Errorf("posted %v, want %v", posted, want)
	}
}

func TestStats(t *testing.T) {
	srv := httptest.NewServer(fakeEngine(new([]string)))
	defer srv.Close()
	c, err := New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if rx, tx, ok, err := c.Stats(ctx, "aaa111"); err != nil || !ok || rx != 1005 || tx != 207 {
		t.Errorf("Stats = %d %d %v %v", rx, tx, ok, err)
	}
	if _, _, ok, err := c.Stats(ctx, "bbb222"); err != nil || ok {
		t.Errorf("Stats without networks = %v %v", ok, err)
	}
	if _, _, _, err := c.Stats(ctx, "missing"); err == nil {
		t.Error("Stats(missing) did not fail")
	}
}

func TestPingAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"Version":"28.5.2"}`)
		case "/containers/json":
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "plain failure")
		case "/containers/bad/json":
			fmt.Fprint(w, `{not json`)
		default:
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"denied"}`)
		}
	}))
	defer srv.Close()
	c, _ := New(srv.URL)
	ctx := context.Background()

	if v, err := c.Ping(ctx); err != nil || v != "28.5.2" {
		t.Fatalf("Ping = %q, %v", v, err)
	}
	if _, err := c.List(ctx); err == nil || !strings.Contains(err.Error(), "plain failure") {
		t.Errorf("List error should carry a non-JSON body: %v", err)
	}
	if _, _, err := c.Inspect(ctx, "bad"); err == nil {
		t.Error("Inspect should fail on invalid JSON")
	}
	if err := c.Stop(ctx, "x", time.Second); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Errorf("Stop error: %v", err)
	}
	if err := c.Events(ctx, func(string, string) {}); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Errorf("Events error: %v", err)
	}

	down, _ := New("tcp://docker")
	down.http.Transport = failingTransport{}
	if _, err := down.Ping(ctx); err == nil {
		t.Error("Ping to a closed port should fail")
	}
	if err := down.Start(ctx, "x"); err == nil {
		t.Error("Start to a closed port should fail")
	}
	if err := down.Events(ctx, func(string, string) {}); err == nil {
		t.Error("Events to a closed port should fail")
	}
}

func TestEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" || !strings.Contains(r.URL.Query().Get("filters"), "container") {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintln(w, `{"Action":"create","Actor":{"ID":"aaa"}}`)
		fmt.Fprintln(w, `garbage`)
		fmt.Fprintln(w, `{"Action":"destroy","Actor":{"ID":"bbb"}}`)
	}))
	defer srv.Close()
	c, _ := New(srv.URL)
	var got []string
	err := c.Events(context.Background(), func(action, id string) { got = append(got, action+" "+id) })
	if err != nil || !slices.Equal(got, []string{"create aaa", "destroy bbb"}) {
		t.Fatalf("Events = %v, %v", got, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Events(ctx, func(string, string) {}); err == nil {
		t.Error("Events with a cancelled context should fail")
	}
}

// failingTransport fails every request without touching the network.
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection refused")
}

func TestUnixSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "dkr") // short: socket paths are limited to ~100 bytes
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"Version":"28.5.2"}`)
	}))
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	c, err := New("unix://" + sock)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := c.Ping(context.Background()); err != nil || v != "28.5.2" {
		t.Fatalf("Ping over the socket = %q, %v", v, err)
	}
}

func TestNewRejectsBadHosts(t *testing.T) {
	for _, bad := range []string{"ssh://host", "://nope"} {
		if _, err := New(bad); err == nil {
			t.Errorf("New(%q) should fail", bad)
		}
	}
}

func TestOnFailover(t *testing.T) {
	for v, want := range map[string]string{"stop": "disable", "Stop\n": "disable", "keep": "keep", "KEEP": "keep", "yes": "", "": ""} {
		c := Container{Labels: map[string]string{LabelOnFailover: v}}
		if got := c.OnFailover(); got != want {
			t.Errorf("%q: %q, want %q", v, got, want)
		}
	}
	if (Container{}).OnFailover() != "" {
		t.Error("no labels")
	}
}
