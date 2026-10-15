// SPDX-License-Identifier: AGPL-3.0-or-later

// Package executor stops and starts containers for failover runs and keeps the
// container inventory in sync with every host.
package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/hosts"
	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// Executor stops and starts containers for runs and keeps the container
// inventory in sync with the hosts.
type Executor struct {
	Hosts       *hosts.Registry
	Store       *store.Store
	Log         *slog.Logger
	Now         func() time.Time
	StopTimeout time.Duration // per container, before Docker kills it
	Parallel    int           // containers stopped or started at once, per host
	HealthWait  time.Duration // max wait for a healthcheck before starting dependents
	PollEvery   time.Duration
	ListTimeout time.Duration // per host, for listing its containers
	OnChange    func()        // called after the inventory or a run changes; may be nil

	mu        sync.Mutex
	lastPrint string         // inventory fingerprint, to notify only on real changes
	missed    map[int64]bool // hosts that could not be reached by the last stop pass
}

// New returns an Executor with the default timings: a 30 s stop timeout, 4
// containers at a time per host, health checks polled every 2 s for up to
// 120 s, and 10 s to list a host.
func New(reg *hosts.Registry, st *store.Store, log *slog.Logger) *Executor {
	return &Executor{
		Hosts: reg, Store: st, Log: log, Now: time.Now,
		StopTimeout: 30 * time.Second, Parallel: 4, HealthWait: 120 * time.Second, PollEvery: 2 * time.Second,
		ListTimeout: 10 * time.Second,
	}
}

// hostKey identifies a container across hosts.
type hostKey struct {
	host int64
	key  string
}

// list lists every host and records the inventory of those that answered.
// down holds the hosts that could not be reached, whose inventory rows are
// kept as they were.
func (e *Executor) list(ctx context.Context) (ws []hosts.Workload, down map[int64]error, err error) {
	down = map[int64]error{}
	var fingerprint strings.Builder
	now := e.Now()
	for _, l := range e.Hosts.List(ctx, e.ListTimeout) {
		if l.Err != nil {
			down[l.Host.ID] = l.Err
			e.Log.Warn("cannot reach host", "host", l.Host.Name, "err", l.Err)
			if err := e.Store.SetHostReached(ctx, l.Host.ID, l.Err.Error(), now); err != nil {
				return nil, nil, err
			}
			fmt.Fprintf(&fingerprint, "%d down;", l.Host.ID)
			continue
		}
		if err := e.Store.SetHostReached(ctx, l.Host.ID, "", now); err != nil {
			return nil, nil, err
		}
		items := make([]store.InventoryItem, 0, len(l.Workloads))
		for _, w := range l.Workloads {
			items = append(items, store.InventoryItem{Key: w.Key(), Project: w.Group, Service: w.Member, Name: w.Name, Label: w.Label})
			fmt.Fprintf(&fingerprint, "%d/%s=%s:%v;", w.Host, w.ID, w.Name, w.Running)
		}
		added, err := e.Store.SyncInventory(ctx, l.Host.ID, items, now)
		if err != nil {
			return nil, nil, err
		}
		if len(added) > 0 {
			e.Log.Info("new containers found", "host", l.Host.Name, "keys", added)
		}
		ws = append(ws, l.Workloads...)
	}
	e.mu.Lock()
	changed := fingerprint.String() != e.lastPrint
	e.lastPrint = fingerprint.String()
	e.mu.Unlock()
	if changed {
		e.changed()
	}
	return ws, down, nil
}

// unreachable describes the hosts that could not be reached.
func (e *Executor) unreachable(down map[int64]error, then string) error {
	var errs []error
	for _, h := range e.Hosts.All() {
		if err, ok := down[h.ID]; ok {
			errs = append(errs, fmt.Errorf("cannot reach host %s (%w); %s", h.Name, err, then))
		}
	}
	return errors.Join(errs...)
}

// Sync refreshes the inventory from every host. It fails if a host could not
// be reached.
func (e *Executor) Sync(ctx context.Context) error {
	_, down, err := e.list(ctx)
	if err != nil {
		return err
	}
	return e.unreachable(down, "its containers are kept as they were")
}

// effective maps every known container to its effective policy.
func (e *Executor) effective(ctx context.Context) (map[hostKey]string, error) {
	rows, err := e.Store.Containers(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[hostKey]string, len(rows))
	for _, r := range rows {
		out[hostKey{r.HostID, r.Key}] = r.Effective
	}
	return out, nil
}

// job is one container to stop or start in a run.
type job struct {
	w        hosts.Workload
	recorded string // the run_actions container ID
	restore  []byte
}

// Stop stops every container whose effective policy is disable, on every host
// at once. It is safe to call again for the same failover: the open run is
// reused, and containers already recorded keep their original was_running.
// A host that cannot be reached is stopped by Retry once it is back. trigger
// is "auto", "manual" or "drill".
func (e *Executor) Stop(ctx context.Context, trigger string, toASN int) error {
	defer e.changed()
	ws, down, err := e.list(ctx)
	if err != nil {
		return err
	}
	effective, err := e.effective(ctx)
	if err != nil {
		return err
	}
	run, err := e.Store.OpenRun(ctx)
	if err != nil {
		return err
	}
	if run == nil {
		if run, err = e.Store.CreateRun(ctx, trigger, toASN, e.Now()); err != nil {
			return err
		}
		if err := e.Store.TakePendingStarts(ctx, run.ID); err != nil {
			return err
		}
	}

	var targets []job
	for _, w := range ws {
		if effective[hostKey{w.Host, w.Key()}] != store.Disable {
			continue
		}
		if err := e.Store.AddRunAction(ctx, run.ID, store.RunAction{
			HostID: w.Host, ContainerID: w.ID, Key: w.Key(), Name: w.Name, WasRunning: w.Running,
		}); err != nil {
			return err
		}
		if w.Running {
			targets = append(targets, job{w: w, recorded: w.ID})
		}
	}
	e.setMissed(down)
	e.Log.Info("failover: stopping containers", "run", run.ID, "count", len(targets), "unreachable_hosts", len(down))

	errs := e.perHost(targets, func(rt hosts.Runtime, js []job) []error {
		ls := layers(js)
		slices.Reverse(ls) // dependents first
		var errs []error
		for _, layer := range ls {
			errs = append(errs, e.each(layer, func(i int) error {
				j := js[i]
				restore, err := rt.Stop(ctx, j.w, e.StopTimeout)
				e.record(j.w, err)
				e.report(e.Store.MarkStopped(ctx, run.ID, j.w.Host, j.recorded, restore, errMsg(err), e.Now()))
				return err
			})...)
		}
		return errs
	})
	errs = append(errs, e.unreachable(down, "its containers are stopped when it is back"))
	return errors.Join(errs...)
}

func (e *Executor) setMissed(down map[int64]error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.missed = map[int64]bool{}
	for id := range down {
		e.missed[id] = true
	}
}

func (e *Executor) anyMissed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.missed) > 0
}

// Start starts the containers the open run stopped, if they were running
// before, then closes the run. Containers on a host that cannot be reached
// stay pending and are started by Retry once it is back. Without an open run
// it does nothing.
func (e *Executor) Start(ctx context.Context) error {
	defer e.changed()
	run, err := e.Store.OpenRun(ctx)
	if err != nil || run == nil {
		return err
	}
	actions, err := e.Store.RunActions(ctx, run.ID)
	if err != nil {
		return err
	}
	ws, down, err := e.list(ctx)
	if err != nil {
		return err
	}
	e.setMissed(nil)
	var owed []store.RunAction
	for _, a := range actions {
		if a.WasRunning && !a.Started {
			owed = append(owed, a)
		}
	}
	e.Log.Info("failback: starting containers", "run", run.ID, "count", len(owed))
	errs := e.startOwed(ctx, run.ID, owed, ws, down)

	status := "completed"
	if len(errs) > 0 {
		status = "failed"
	}
	e.report(e.Store.CloseRun(ctx, run.ID, status, e.Now()))
	return errors.Join(errs...)
}

// startOwed starts the given actions of a run, finding each container by its
// ID or, if it was re-created, by its key. Actions on a host that cannot be
// reached are marked pending.
func (e *Executor) startOwed(ctx context.Context, runID int64, owed []store.RunAction, ws []hosts.Workload, down map[int64]error) []error {
	byID := map[hostKey]hosts.Workload{}
	byKey := map[hostKey][]hosts.Workload{}
	for _, w := range ws {
		byID[hostKey{w.Host, w.ID}] = w
		byKey[hostKey{w.Host, w.Key()}] = append(byKey[hostKey{w.Host, w.Key()}], w)
	}
	var targets []job
	var errs []error
	pending := map[int64]bool{}
	seen := map[hostKey]bool{}
	for _, a := range owed {
		if _, ok := e.Hosts.Get(a.HostID); !ok || down[a.HostID] != nil {
			pending[a.HostID] = true
			e.report(e.Store.MarkPending(ctx, runID, a.HostID, a.ContainerID, "waiting for the host to be reachable"))
			continue
		}
		found := []hosts.Workload{}
		if w, ok := byID[hostKey{a.HostID, a.ContainerID}]; ok {
			found = append(found, w)
		} else {
			found = byKey[hostKey{a.HostID, a.Key}]
		}
		if len(found) == 0 {
			err := fmt.Errorf("%s no longer exists", a.Name)
			errs = append(errs, err)
			e.report(e.Store.MarkStarted(ctx, runID, a.HostID, a.ContainerID, err.Error(), e.Now()))
			continue
		}
		for _, w := range found {
			if !seen[hostKey{w.Host, w.ID}] {
				seen[hostKey{w.Host, w.ID}] = true
				targets = append(targets, job{w: w, recorded: a.ContainerID, restore: a.Restore})
			}
		}
	}

	errs = append(errs, e.perHost(targets, func(rt hosts.Runtime, js []job) []error {
		var errs []error
		ls := layers(js)
		for n, layer := range ls {
			errs = append(errs, e.each(layer, func(i int) error {
				j := js[i]
				err := rt.Start(ctx, j.w, j.restore)
				e.record(j.w, err)
				e.report(e.Store.MarkStarted(ctx, runID, j.w.Host, j.recorded, errMsg(err), e.Now()))
				return err
			})...)
			if n < len(ls)-1 {
				e.waitHealthy(ctx, rt, js, layer)
			}
		}
		return errs
	})...)
	for _, h := range e.Hosts.All() {
		if pending[h.ID] {
			errs = append(errs, fmt.Errorf("cannot reach host %s; its containers are started when it is back", h.Name))
			delete(pending, h.ID)
		}
	}
	for id := range pending {
		errs = append(errs, fmt.Errorf("host %d is not connected; its containers are started when it is back", id))
	}
	return errs
}

// Retry catches up with hosts that could not be reached: during a failover it
// stops their containers once they answer again, and afterwards it starts
// the containers still owed by an ended run. It does nothing when nothing is
// owed.
func (e *Executor) Retry(ctx context.Context) error {
	run, err := e.Store.OpenRun(ctx)
	if err != nil {
		return err
	}
	if run != nil {
		if !e.anyMissed() {
			return nil
		}
		return e.Stop(ctx, run.Trigger, run.ToASN)
	}
	pend, err := e.Store.PendingStarts(ctx)
	if err != nil || len(pend) == 0 {
		return err
	}
	defer e.changed()
	ws, down, err := e.list(ctx)
	if err != nil {
		return err
	}
	byRun := map[int64][]store.RunAction{}
	var runs []int64
	for _, p := range pend {
		if down[p.HostID] != nil {
			continue // still unreachable: try again next time
		}
		if _, ok := byRun[p.RunID]; !ok {
			runs = append(runs, p.RunID)
		}
		byRun[p.RunID] = append(byRun[p.RunID], p.RunAction)
	}
	var errs []error
	for _, id := range runs {
		e.Log.Info("host back: starting the containers still owed", "run", id, "count", len(byRun[id]))
		errs = append(errs, e.startOwed(ctx, id, byRun[id], ws, down)...)
	}
	return errors.Join(errs...)
}

// perHost runs fn for each host's jobs, every host at once.
func (e *Executor) perHost(js []job, fn func(rt hosts.Runtime, js []job) []error) []error {
	byHost := map[int64][]job{}
	for _, j := range js {
		byHost[j.w.Host] = append(byHost[j.w.Host], j)
	}
	var mu sync.Mutex
	var errs []error
	var wg sync.WaitGroup
	for id, hjs := range byHost {
		h, ok := e.Hosts.Get(id)
		if !ok {
			continue
		}
		wg.Go(func() {
			herrs := fn(h.Runtime, hjs)
			mu.Lock()
			errs = append(errs, herrs...)
			mu.Unlock()
		})
	}
	wg.Wait()
	return errs
}

func errMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (e *Executor) record(w hosts.Workload, err error) {
	if err != nil {
		e.Log.Error("container action failed", "host", w.Host, "container", w.Name, "err", err)
		return
	}
	e.Log.Info("container action done", "host", w.Host, "container", w.Name)
}

// each runs fn for every index, at most e.Parallel at a time.
func (e *Executor) each(idx []int, fn func(int) error) []error {
	sem := make(chan struct{}, max(1, e.Parallel))
	var mu sync.Mutex
	var errs []error
	var wg sync.WaitGroup
	for _, i := range idx {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := fn(i); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return errs
}

// waitHealthy waits until every container with a healthcheck reports healthy,
// up to HealthWait. Containers without a healthcheck count as ready.
func (e *Executor) waitHealthy(ctx context.Context, rt hosts.Runtime, js []job, idx []int) {
	deadline := time.Now().Add(e.HealthWait)
	for _, i := range idx {
		w := js[i].w
		for {
			running, health, err := rt.Inspect(ctx, w)
			if err != nil || !running || health == "" || health == "healthy" {
				break
			}
			if time.Now().After(deadline) {
				e.Log.Warn("healthcheck wait timed out; starting dependents anyway", "container", w.Name, "health", health)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(e.PollEvery):
			}
		}
	}
}

// Apply makes the open run match the current policies, for changes made
// during a failover: newly disabled containers are stopped, and containers no
// longer disabled are started if Drawbridge stopped them. Without an open run
// it does nothing.
func (e *Executor) Apply(ctx context.Context) error {
	defer e.changed()
	run, err := e.Store.OpenRun(ctx)
	if err != nil || run == nil {
		return err
	}
	var errs []error
	errs = append(errs, e.Stop(ctx, run.Trigger, run.ToASN))

	actions, err := e.Store.RunActions(ctx, run.ID)
	if err != nil {
		return err
	}
	effective, err := e.effective(ctx)
	if err != nil {
		return err
	}
	for _, a := range actions {
		if !a.WasRunning || !a.Stopped || a.Started || effective[hostKey{a.HostID, a.Key}] == store.Disable {
			continue
		}
		h, ok := e.Hosts.Get(a.HostID)
		if !ok {
			continue // started at the failback, or once the host is back
		}
		w := hosts.Workload{Host: a.HostID, ID: a.ContainerID, Name: a.Name}
		err := h.Runtime.Start(ctx, w, a.Restore)
		e.record(w, err)
		e.report(e.Store.MarkStarted(ctx, run.ID, a.HostID, a.ContainerID, errMsg(err), e.Now()))
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (e *Executor) changed() {
	if e.OnChange != nil {
		e.OnChange()
	}
}

func (e *Executor) report(err error) {
	if err != nil {
		e.Log.Error("executor error", "err", err)
	}
}

// layers groups job indices by Compose dependency depth: layer 0 depends on
// nothing in the set, layer 1 only on layer 0, and so on. Dependencies only
// link containers of the same stack on the same host; those outside the set,
// and cycles, are ignored.
func layers(js []job) [][]int {
	type node struct {
		host          int64
		group, member string
	}
	byMember := map[node][]int{}
	for i, j := range js {
		if j.w.Member != "" {
			n := node{j.w.Host, j.w.Group, j.w.Member}
			byMember[n] = append(byMember[n], i)
		}
	}
	depth := make([]int, len(js))
	state := make([]int, len(js)) // 0 new, 1 visiting, 2 done
	var visit func(i int) int
	visit = func(i int) int {
		switch state[i] {
		case 1:
			return -1 // cycle: ignore this edge
		case 2:
			return depth[i]
		}
		state[i] = 1
		d := 0
		w := js[i].w
		for _, m := range w.DependsOn {
			for _, k := range byMember[node{w.Host, w.Group, m}] {
				if dd := visit(k); dd >= 0 {
					d = max(d, dd+1)
				}
			}
		}
		state[i], depth[i] = 2, d
		return d
	}
	var layers [][]int
	for i := range js {
		d := visit(i)
		for len(layers) <= d {
			layers = append(layers, nil)
		}
		layers[d] = append(layers[d], i)
	}
	return layers
}
