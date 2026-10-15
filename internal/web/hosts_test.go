// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/docker"
	"github.com/zjcochran/wan-drawbridge/internal/hosts"
	"github.com/zjcochran/wan-drawbridge/internal/store"
	"github.com/zjcochran/wan-drawbridge/internal/wan"
)

// addHost adds a second host, "nas", with its own "media" stack; down makes
// it unreachable.
func addHost(t *testing.T, e *env, down bool) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := e.st.AddHost(ctx, "nas", "agent", "docker", "test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var d hosts.DockerAPI = fakeDocker{cs: []docker.Container{container("player", true), container("fetcher", false)}}
	if down {
		d = downDocker{}
	}
	e.ui.Hosts.Set(hosts.Host{ID: id, Name: "nas", Runtime: hosts.Local{Docker: d}})
	if _, err := e.st.SyncInventory(ctx, id, []store.InventoryItem{
		{Key: "media/player", Project: "media", Service: "player", Name: "media-player-1"},
		{Key: "media/fetcher", Project: "media", Service: "fetcher", Name: "media-fetcher-1"},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestOneHostShowsNoHostColumn(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	for _, p := range []string{"/stacks", "/containers"} {
		if _, body := c.get(p); strings.Contains(body, "<th>Host</th>") || strings.Contains(body, `aria-label="Host"`) {
			t.Errorf("%s shows hosts with only one", p)
		}
	}
}

func TestStacksPerHost(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	id := addHost(t, e, false)
	nas := strconv.FormatInt(id, 10)

	_, body := c.get("/stacks")
	if !strings.Contains(body, "<th>Host</th>") || !strings.Contains(body, `<option value="`+nas+`" >nas</option>`) ||
		!strings.Contains(body, `hx-post="/stacks/`+nas+`/media/policy"`) || !strings.Contains(body, `hx-post="/stacks/1/media/policy"`) {
		t.Fatalf("stacks page:\n%s", body)
	}
	if _, body := c.get("/stacks?host=" + nas); strings.Contains(body, `/stacks/1/media/policy`) || !strings.Contains(body, "1 running of 2") {
		t.Errorf("host filter on stacks:\n%s", body)
	}

	// Turning the nas stack on leaves the local one alone.
	if resp, _ := c.htmx("/stacks/"+nas+"/media/policy", url.Values{"disable": {"on"}, "host": {nas}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("stack policy: %d", resp.StatusCode)
	}
	if resp, body := c.get("/stacks/" + nas + "/media/containers"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "media-fetcher-1") {
		t.Errorf("nas stack containers: %d %s", resp.StatusCode, body)
	}
	if _, body := c.get("/stacks/1/media/containers"); strings.Contains(body, "media-fetcher-1") {
		t.Error("the local stack lists the nas containers")
	}
	evs, _ := e.st.Events(context.Background(), store.EventFilter{Type: "stack_policy", Limit: 1})
	if len(evs) != 1 || evs[0].Payload["host"] != "nas" {
		t.Errorf("event = %+v", evs)
	}
	if resp, _ := c.get("/stacks/nas/media/containers"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a host that is not a number: %d", resp.StatusCode)
	}
	if resp, _ := c.htmx("/stacks/nas/media/policy", url.Values{"disable": {"on"}}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("policy for a host that is not a number: %d", resp.StatusCode)
	}
}

func TestContainersPerHost(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	nas := strconv.FormatInt(addHost(t, e, false), 10)
	_, body := c.get("/containers?host=" + nas)
	if !strings.Contains(body, `value="`+nas+`:media/player"`) || strings.Contains(body, `value="1:media/player"`) ||
		!strings.Contains(body, `<a href="/containers?host=`+nas+`">nas</a>`) {
		t.Fatalf("containers filtered by host:\n%s", body)
	}

	// Per-container and bulk settings name their host.
	if resp, _ := c.htmx("/containers/policy", url.Values{"container_host": {nas}, "key": {"media/player"}, "policy": {"keep"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("container policy: %d", resp.StatusCode)
	}
	if resp, _ := c.htmx("/containers/bulk", url.Values{"keys": {"1:dns", nas + ":media/fetcher"}, "bulk_policy": {"keep"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("bulk: %d", resp.StatusCode)
	}
	rows, _ := e.st.Containers(context.Background())
	got := map[string]string{}
	for _, r := range rows {
		got[r.Host+":"+r.Key] = r.Policy
	}
	if got["nas:media/player"] != store.Keep || got["local:media/player"] != store.Inherit || got["nas:media/fetcher"] != store.Keep || got["local:dns"] != store.Keep {
		t.Errorf("policies = %v", got)
	}
	for _, bad := range []url.Values{{"keys": {"media/player"}}, {"keys": {"x:dns"}}} {
		bad.Set("bulk_policy", "keep")
		if resp, _ := c.htmx("/containers/bulk", bad); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("bulk %v: %d", bad, resp.StatusCode)
		}
	}
}

func TestOfflineHost(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	addHost(t, e, true)
	if _, body := c.get("/stacks"); !strings.Contains(body, `<tr class="dim">`) || !strings.Contains(body, "host offline") {
		t.Errorf("stacks with an offline host:\n%s", body)
	}
	if _, body := c.get("/containers"); !strings.Contains(body, "host offline") || !strings.Contains(body, "media-fetcher-1") {
		t.Errorf("containers with an offline host:\n%s", body)
	}
}

func TestHostsOnDashboardReportsAPIAndMetrics(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	ctx := context.Background()
	id := addHost(t, e, false)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// An earlier run still owes a start on nas; the open run stopped a
	// container there.
	old, _ := e.st.CreateRun(ctx, "auto", 0, time.Now())
	must(e.st.AddRunAction(ctx, old.ID, store.RunAction{HostID: id, ContainerID: "id-fetcher", Key: "media/fetcher", Name: "media-fetcher-1", WasRunning: true}))
	must(e.st.MarkPending(ctx, old.ID, id, "id-fetcher", "waiting"))
	must(e.st.CloseRun(ctx, old.ID, "completed", time.Now()))
	if _, body := c.get("/partials/status"); !strings.Contains(body, "1 container on nas will be started again once Drawbridge can reach it.") ||
		strings.Contains(body, "Nothing.") {
		t.Errorf("owed starts on the dashboard:\n%s", body)
	}
	run, _ := e.st.CreateRun(ctx, "auto", 0, time.Now())
	must(e.st.AddRunAction(ctx, run.ID, store.RunAction{HostID: id, ContainerID: "id-player", Key: "media/player", Name: "media-player-1", WasRunning: true}))
	must(e.st.MarkStopped(ctx, run.ID, id, "id-player", nil, "", time.Now()))
	if _, body := c.get("/partials/status"); !strings.Contains(body, "media-player-1 on nas") {
		t.Errorf("host on the dashboard:\n%s", body)
	}

	token, _ := e.st.CreateAPIToken(ctx, "test", "admin", time.Now())
	resp, st := apiGet(t, e, token)
	if resp.StatusCode != http.StatusOK || len(st.Hosts) != 2 || st.Hosts[1].Name != "nas" || st.Hosts[1].Containers != 2 || st.Hosts[1].Stopped != 1 || st.Hosts[0].Containers != 3 {
		t.Errorf("api hosts = %+v", st.Hosts)
	}

	must(e.st.SetHostReached(ctx, id, "refused", time.Now()))
	e.ui.MetricsToken = "scrape-me"
	if _, body := e.client().do("GET", "/metrics", nil, "Authorization", "Bearer scrape-me"); !strings.Contains(body, `drawbridge_host_up{host="local"} 1`) ||
		!strings.Contains(body, `drawbridge_host_up{host="nas"} 0`) {
		t.Errorf("metrics:\n%s", body)
	}

	// Usage during an outage names the host.
	must(e.st.SaveState(ctx, store.WANState{Machine: wan.Machine{State: wan.Failover}, Since: time.Now(), LearningStartedAt: time.Now()}))
	outage, _ := e.st.OpenOutage(ctx)
	for _, rx := range []int64{0, 2048} {
		must(e.st.RecordUsage(ctx, outage, id, "media/player", "id-player", rx, 0, time.Now()))
	}
	if _, body := c.get("/reports"); !strings.Contains(body, "<td>media/player</td><td>nas</td>") {
		t.Errorf("reports page:\n%s", body)
	}
	if _, body := c.get("/reports/outages/" + strconv.FormatInt(outage, 10)); !strings.Contains(body, "<td>media/player</td><td>nas</td>") {
		t.Errorf("outage page:\n%s", body)
	}
}

func TestSwarmServicesListed(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	id := addHost(t, e, true) // offline: its stored rows are all listed
	if _, err := e.st.SyncInventory(context.Background(), id, []store.InventoryItem{
		{Key: "media/player", Project: "media", Service: "player", Name: "media_player", Kind: "service"},
		{Key: "media/deployment/web", Project: "media", Service: "deployment/web", Name: "web", Kind: "deployment",
			Warning: "the HorizontalPodAutoscaler web-hpa scales it back up"},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, body := c.get("/containers")
	if !strings.Contains(body, `<strong>media_player</strong> <span class="badge muted">Swarm service</span>`) {
		t.Errorf("a Swarm service is not marked:\n%s", body)
	}
	if !strings.Contains(body, `<strong>web</strong> <span class="badge muted">deployment</span>`) ||
		!strings.Contains(body, `<div class="sub warn-text">the HorizontalPodAutoscaler web-hpa scales it back up</div>`) {
		t.Errorf("a Kubernetes workload or its warning is not shown:\n%s", body)
	}
}
