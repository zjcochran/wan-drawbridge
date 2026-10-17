// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/executor"
	"github.com/zjcochran/wan-drawbridge/internal/hosts"
	"github.com/zjcochran/wan-drawbridge/internal/store"
)

type fakePlanner struct {
	plan executor.Plan
	err  error
}

func (f fakePlanner) Plan(context.Context) (executor.Plan, error) { return f.plan, f.err }

func TestPreviewPage(t *testing.T) {
	e := newEnv(t)
	v := e.viewerClient()
	if resp, _ := v.get("/preview"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("without a planner: %d", resp.StatusCode)
	}
	e.ui.Planner = fakePlanner{plan: executor.Plan{Hosts: []executor.HostPlan{
		{HostID: store.LocalHost, Keep: 1,
			Steps: [][]hosts.Workload{{{Name: "media-web-1", Group: "media"}}, {{Name: "media-cache-1", Group: "media"}, {Name: "media-db-1", Group: "media"}}},
			Idle:  []hosts.Workload{{Name: "media-worker-1"}}},
		{HostID: 7, Err: errors.New("connection refused")},
	}}}
	_, page := v.get("/preview")
	for _, want := range []string{"<strong>3</strong> containers would be stopped, on every host at once",
		"<strong>media-cache-1</strong> <span class=\"sub\">media</span>, <strong>media-db-1</strong>",
		"Already stopped, left alone and not started at failback: media-worker-1.",
		"1 running container keeps running.", "host 7", "Cannot be reached (connection refused)"} {
		if !strings.Contains(page, want) {
			t.Errorf("the preview lacks %q", want)
		}
	}
	if strings.Contains(page, "A failover is in effect") {
		t.Error("the preview says a failover is in effect")
	}
	if _, dash := v.get("/"); !strings.Contains(dash, `href="/preview"`) {
		t.Error("the dashboard does not link the preview")
	}
}

func TestPreviewDuringFailover(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.st.CreateRun(ctx, "auto", 64510, time.Now())
	e.ui.Planner = fakePlanner{plan: executor.Plan{Hosts: []executor.HostPlan{{HostID: store.LocalHost}}}}
	c := e.admin()
	_, page := c.get("/preview")
	if !strings.Contains(page, "A failover is in effect") || !strings.Contains(page, "Nothing would be stopped.") {
		t.Error("the preview during a failover is wrong")
	}
	e.ui.Planner = fakePlanner{err: errors.New("database is locked")}
	if resp, _ := c.get("/preview"); resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("planner error: %d", resp.StatusCode)
	}
}
