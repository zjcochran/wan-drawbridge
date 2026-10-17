// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/hosts"
	"github.com/zjcochran/wan-drawbridge/internal/store"
)

func names(ws []hosts.Workload) []string {
	out := []string{}
	for _, w := range ws {
		out = append(out, w.Name)
	}
	return out
}

func TestPlan(t *testing.T) {
	ctx := context.Background()
	f := &fakeDocker{}
	f.add("db", "app", "db", true, "")
	f.add("cache", "app", "cache", true, "")
	f.add("web", "app", "web", true, "db:service_started:false,cache:service_started:false")
	f.add("worker", "app", "worker", false, "db:service_started:false") // already stopped
	f.add("dns", "", "", true, "")                                      // standalone, kept
	e, st := setup(t, f)
	if err := st.SetStackPolicy(ctx, store.LocalHost, "app", true, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	p, err := e.Plan(ctx)
	if err != nil || len(p.Hosts) != 1 {
		t.Fatalf("Plan = %+v, %v", p, err)
	}
	h := p.Hosts[0]
	if len(h.Steps) != 2 || len(h.Steps[0]) != 1 || h.Steps[0][0].Name != "web" ||
		names(h.Steps[1])[0] != "cache" || names(h.Steps[1])[1] != "db" {
		t.Errorf("steps = %v", h.Steps)
	}
	if names(h.Idle)[0] != "worker" || h.Keep != 1 || h.Err != nil {
		t.Errorf("idle %v, keep %d, err %v", names(h.Idle), h.Keep, h.Err)
	}
	if len(f.ops) != 0 {
		t.Errorf("Plan changed something: %v", f.ops)
	}
	if run, _ := st.OpenRun(ctx); run != nil {
		t.Error("Plan opened a run")
	}
}

func TestPlanWithAHostDown(t *testing.T) {
	ctx := context.Background()
	local, nas := twoHosts()
	e, _, id := setupTwo(t, local, nas)
	nas.setDown(errors.New("connection refused"))
	p, err := e.Plan(ctx)
	if err != nil || len(p.Hosts) != 2 {
		t.Fatalf("Plan = %+v, %v", p, err)
	}
	if p.Hosts[0].HostID != store.LocalHost || len(p.Hosts[0].Steps) != 1 {
		t.Errorf("local = %+v", p.Hosts[0])
	}
	if p.Hosts[1].HostID != id || p.Hosts[1].Err == nil || len(p.Hosts[1].Steps) != 0 {
		t.Errorf("nas = %+v", p.Hosts[1])
	}
}
