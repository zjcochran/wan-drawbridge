// SPDX-License-Identifier: AGPL-3.0-or-later

package proxmox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePVE is a Proxmox API with two LXC containers and a VM.
type fakePVE struct {
	mu       sync.Mutex
	status   map[int]string // vmid → status
	calls    []string
	taskPoll int // polls before a task reports stopped
	failTask bool
}

func (f *fakePVE) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "PVEAPIToken=drawbridge@pve!agent=s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"invalid token"}`)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, "/api2/json")
		f.calls = append(f.calls, r.Method+" "+p)
		switch {
		case p == "/cluster/resources" && r.URL.Query().Get("type") == "vm":
			fmt.Fprintf(w, `{"data":[
				{"id":"lxc/101","type":"lxc","node":"pve1","vmid":101,"name":"db","status":"%s","pool":"media","tags":"drawbridge-stop;web","netin":1000,"netout":20},
				{"id":"lxc/102","type":"lxc","node":"pve2","vmid":102,"name":"app","status":"%s","pool":"media"},
				{"id":"qemu/200","type":"qemu","node":"pve1","vmid":200,"name":"vm","status":"running"}]}`, f.status[101], f.status[102])
		case p == "/nodes/pve1/lxc/101/config":
			fmt.Fprint(w, `{"data":{"hostname":"db","startup":"up=30,order=2"}}`)
		case p == "/nodes/pve2/lxc/102/config":
			fmt.Fprint(w, `{"data":{"hostname":"app"}}`)
		case p == "/cluster/ha/resources":
			fmt.Fprint(w, `{"data":[{"sid":"ct:102","state":"started"}]}`)
		case strings.HasSuffix(p, "/status/shutdown") && r.Method == http.MethodPost:
			if r.FormValue("timeout") != "30" || r.FormValue("forceStop") != "1" {
				t.Errorf("shutdown form = %v", r.Form)
			}
			f.status[101] = "stopped"
			fmt.Fprint(w, `{"data":"UPID:pve1:1:shutdown"}`)
		case strings.HasSuffix(p, "/status/start") && r.Method == http.MethodPost:
			f.status[101] = "running"
			fmt.Fprint(w, `{"data":"UPID:pve1:2:start"}`)
		case strings.HasPrefix(p, "/nodes/pve1/tasks/"):
			if f.taskPoll > 0 {
				f.taskPoll--
				fmt.Fprint(w, `{"data":{"status":"running"}}`)
				return
			}
			exit := "OK"
			if f.failTask {
				exit = "command 'lxc-start' failed: exit code 1"
			}
			fmt.Fprintf(w, `{"data":{"status":"stopped","exitstatus":%q}}`, exit)
		case p == "/cluster/ha/resources/ct:102" && r.Method == http.MethodPut:
			f.status[102] = map[string]string{"stopped": "stopped", "started": "running"}[r.FormValue("state")]
			fmt.Fprint(w, `{"data":null}`)
		default:
			w.WriteHeader(http.StatusNotImplemented)
			fmt.Fprint(w, `{"data":null}`)
		}
	})
}

func newPVE(t *testing.T) (*fakePVE, *httptest.Server) {
	f := &fakePVE{status: map[int]string{101: "running", 102: "running"}}
	srv := httptest.NewTLSServer(f.handler(t))
	t.Cleanup(srv.Close)
	return f, srv
}

func client(t *testing.T, srv *httptest.Server, fp string) *Client {
	t.Helper()
	c, err := New(srv.URL, "drawbridge@pve!agent", "s3cret", fp)
	if err != nil {
		t.Fatal(err)
	}
	c.PollEvery = time.Millisecond
	return c
}

func TestNewChecksSettings(t *testing.T) {
	for _, c := range []struct{ url, id, secret string }{
		{"http://pve:8006", "a@pve!b", "x"},
		{"https://", "a@pve!b", "x"},
		{"https://pve:8006", "a@pve", "x"},
		{"https://pve:8006", "a!b", "x"},
		{"https://pve:8006", "a@pve!b", ""},
	} {
		if _, err := New(c.url, c.id, c.secret, "AA"); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
}

func TestFingerprintPinning(t *testing.T) {
	ctx := context.Background()
	_, srv := newPVE(t)
	fp := Fingerprint(srv.Certificate().Raw)
	if len(fp) != 95 || strings.Count(fp, ":") != 31 {
		t.Fatalf("fingerprint %q", fp)
	}
	_, err := client(t, srv, "").Guests(ctx)
	if !errors.Is(err, ErrNoFingerprint) || !strings.Contains(err.Error(), fp) {
		t.Errorf("without a fingerprint: %v", err)
	}
	if _, err := client(t, srv, strings.Repeat("AB:", 31)+"AB").Guests(ctx); err == nil || !strings.Contains(err.Error(), "not PROXMOX_FINGERPRINT") {
		t.Errorf("wrong fingerprint: %v", err)
	}
	// Lower case, without colons, is the same fingerprint.
	if _, err := client(t, srv, strings.ToLower(strings.ReplaceAll(fp, ":", ""))).Guests(ctx); err != nil {
		t.Errorf("right fingerprint: %v", err)
	}
	bad, _ := New(srv.URL, "drawbridge@pve!agent", "wrong", fp)
	if _, err := bad.Guests(ctx); err == nil || !strings.Contains(err.Error(), "invalid token") {
		t.Errorf("wrong token: %v", err)
	}
}

func TestGuestsAndOrder(t *testing.T) {
	ctx := context.Background()
	_, srv := newPVE(t)
	c := client(t, srv, Fingerprint(srv.Certificate().Raw))
	gs, err := c.Guests(ctx)
	if err != nil || len(gs) != 2 {
		t.Fatalf("guests = %+v, %v (the VM must be left out)", gs, err)
	}
	if g := gs[0]; g.ID != "lxc/101" || g.Node != "pve1" || g.VMID != 101 || g.Pool != "media" || g.NetIn != 1000 ||
		strings.Join(g.TagList(), ",") != "drawbridge-stop,web" {
		t.Errorf("guest = %+v", g)
	}
	if o, err := c.StartupOrder(ctx, gs[0]); err != nil || o != 2 {
		t.Errorf("order = %d, %v", o, err)
	}
	if o, err := c.StartupOrder(ctx, gs[1]); err != nil || o != 0 {
		t.Errorf("no order = %d, %v", o, err)
	}
	ha, err := c.HAManaged(ctx)
	if err != nil || !ha["ct:102"] || ha["ct:101"] {
		t.Errorf("ha = %v, %v", ha, err)
	}
}

func TestStopAndStart(t *testing.T) {
	ctx := context.Background()
	f, srv := newPVE(t)
	c := client(t, srv, Fingerprint(srv.Certificate().Raw))
	gs, _ := c.Guests(ctx)
	f.taskPoll = 2
	if err := c.Shutdown(ctx, gs[0], 30*time.Second); err != nil || f.status[101] != "stopped" {
		t.Fatalf("shutdown: %v", err)
	}
	if err := c.Start(ctx, gs[0]); err != nil || f.status[101] != "running" {
		t.Fatalf("start: %v", err)
	}
	f.failTask = true
	if err := c.Start(ctx, gs[0]); err == nil || !strings.Contains(err.Error(), "lxc-start") {
		t.Errorf("a failed task: %v", err)
	}
	// Through HA: the state is asked for, then waited for.
	if err := c.SetHAState(ctx, gs[1], "stopped", time.Second); err != nil || f.status[102] != "stopped" {
		t.Fatalf("HA stop: %v", err)
	}
	if err := c.SetHAState(ctx, gs[1], "started", time.Second); err != nil || f.status[102] != "running" {
		t.Fatalf("HA start: %v", err)
	}
	if err := c.SetHAState(ctx, gs[0], "stopped", 10*time.Millisecond); err == nil {
		t.Error("an HA change for a container HA does not manage succeeded")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := c.wait(cctx, "pve1", "UPID:x"); err == nil {
		t.Error("waiting went on after the context ended")
	}
}
