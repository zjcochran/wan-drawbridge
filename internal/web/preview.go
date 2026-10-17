// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"strconv"

	"github.com/zjcochran/wan-drawbridge/internal/executor"
	"github.com/zjcochran/wan-drawbridge/internal/hosts"
)

// Planner works out what a failover would do now.
type Planner interface {
	Plan(ctx context.Context) (executor.Plan, error)
}

// previewHost is one host on the failover preview.
type previewHost struct {
	Name  string
	Steps [][]hosts.Workload
	Idle  []hosts.Workload
	Keep  int
	Error string // the host could not be reached
}

// Stops counts the containers that would be stopped.
func (p previewHost) Stops() int {
	n := 0
	for _, s := range p.Steps {
		n += len(s)
	}
	return n
}

type previewData struct {
	Hosts       []previewHost
	MultiHost   bool
	InFailover  bool // a run is open: the containers it stopped are already stopped
	NeedsReview int
	Stops       int
}

func (s *Server) previewPage(w http.ResponseWriter, r *http.Request) {
	if s.Planner == nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	plan, err := s.Planner.Plan(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d := previewData{MultiHost: len(plan.Hosts) > 1}
	if run, err := s.Store.OpenRun(ctx); err != nil {
		s.fail(w, r, err)
		return
	} else if run != nil {
		d.InFailover = true
	}
	rows, err := s.Store.Containers(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, c := range rows {
		if c.NeedsReview {
			d.NeedsReview++
		}
	}
	for _, hp := range plan.Hosts {
		p := previewHost{Steps: hp.Steps, Idle: hp.Idle, Keep: hp.Keep, Name: hostName(s.Hosts, hp.HostID)}
		if hp.Err != nil {
			p.Error = hp.Err.Error()
		}
		d.Stops += p.Stops()
		d.Hosts = append(d.Hosts, p)
	}
	s.page(w, r, "preview", "Failover preview", d)
}

func hostName(reg *hosts.Registry, id int64) string {
	if reg != nil {
		if h, ok := reg.Get(id); ok {
			return h.Name
		}
	}
	return "host " + strconv.FormatInt(id, 10)
}
