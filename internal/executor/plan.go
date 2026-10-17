// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"context"
	"slices"

	"github.com/zjcochran/wan-drawbridge/internal/hosts"
	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// Plan is what a failover would do now, per host.
type Plan struct {
	Hosts []HostPlan // in host ID order
}

// HostPlan is what a failover would do on one host.
type HostPlan struct {
	HostID int64
	// Steps are the containers that would be stopped, in order: each step
	// starts once the one before it has stopped. Dependents come first.
	Steps [][]hosts.Workload
	// Idle are set to stop but already stopped: they are recorded and left
	// alone, and not started at failback.
	Idle []hosts.Workload
	Keep int   // running containers that would keep running
	Err  error // the host could not be reached: it is stopped when it is back
}

// Plan works out what Stop would do now, without changing anything. Like
// Sync, it refreshes the inventory first.
func (e *Executor) Plan(ctx context.Context) (Plan, error) {
	ws, down, err := e.list(ctx)
	if err != nil {
		return Plan{}, err
	}
	effective, err := e.effective(ctx)
	if err != nil {
		return Plan{}, err
	}
	byHost := map[int64]*HostPlan{}
	jobs := map[int64][]job{}
	for _, h := range e.Hosts.All() {
		byHost[h.ID] = &HostPlan{HostID: h.ID, Err: down[h.ID]}
	}
	for _, w := range ws {
		p := byHost[w.Host]
		if p == nil {
			continue
		}
		switch {
		case effective[hostKey{w.Host, w.Key()}] != store.Disable:
			if w.Running {
				p.Keep++
			}
		case w.Running:
			jobs[w.Host] = append(jobs[w.Host], job{w: w})
		default:
			p.Idle = append(p.Idle, w)
		}
	}
	var out Plan
	for _, p := range byHost {
		js := jobs[p.HostID]
		ls := layers(js)
		slices.Reverse(ls) // dependents first, as Stop does
		for _, layer := range ls {
			step := make([]hosts.Workload, 0, len(layer))
			for _, i := range layer {
				step = append(step, js[i].w)
			}
			slices.SortFunc(step, byName)
			p.Steps = append(p.Steps, step)
		}
		slices.SortFunc(p.Idle, byName)
		out.Hosts = append(out.Hosts, *p)
	}
	slices.SortFunc(out.Hosts, func(a, b HostPlan) int { return int(a.HostID - b.HostID) })
	return out, nil
}

func byName(a, b hosts.Workload) int {
	switch {
	case a.Name < b.Name:
		return -1
	case a.Name > b.Name:
		return 1
	}
	return 0
}
