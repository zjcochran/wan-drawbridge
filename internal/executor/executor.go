// SPDX-License-Identifier: AGPL-3.0-or-later

// Package executor stops and starts containers for failover runs and keeps the
// container inventory in sync with Docker.
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

	"github.com/zjcochran/wan-drawbridge/internal/docker"
	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// Docker is the part of the Docker client the executor uses.
type Docker interface {
	List(ctx context.Context) ([]docker.Container, error)
	Inspect(ctx context.Context, id string) (running bool, health docker.Health, err error)
	Stop(ctx context.Context, id string, timeout time.Duration) error
	Start(ctx context.Context, id string) error
}

type Executor struct {
	Docker      Docker
	Store       *store.Store
	Log         *slog.Logger
	Now         func() time.Time
	SelfID      string        // this container's ID or ID prefix (its hostname)
	StopTimeout time.Duration // per container, before Docker kills it
	Parallel    int           // containers stopped or started at once
	HealthWait  time.Duration // max wait for a healthcheck before starting dependents
	PollEvery   time.Duration
	OnChange    func() // called after the inventory or a run changes; may be nil

	mu        sync.Mutex
	lastPrint string // inventory fingerprint, to notify only on real changes
}

func New(d Docker, st *store.Store, log *slog.Logger, selfID string) *Executor {
	return &Executor{
		Docker: d, Store: st, Log: log, Now: time.Now, SelfID: selfID,
		StopTimeout: 30 * time.Second, Parallel: 4, HealthWait: 120 * time.Second, PollEvery: 2 * time.Second,
	}
}

// isSelf reports whether c is Drawbridge's own container, which is never touched.
func (e *Executor) isSelf(c docker.Container) bool {
	if c.Labels[docker.LabelSelf] == "true" {
		return true
	}
	return len(e.SelfID) >= 12 && strings.HasPrefix(c.ID, e.SelfID)
}

// list returns Docker's containers without Drawbridge itself, after recording
// them in the inventory.
func (e *Executor) list(ctx context.Context) ([]docker.Container, error) {
	all, err := e.Docker.List(ctx)
	if err != nil {
		return nil, err
	}
	var cs []docker.Container
	var items []store.InventoryItem
	var print strings.Builder
	for _, c := range all {
		if e.isSelf(c) {
			continue
		}
		cs = append(cs, c)
		items = append(items, store.InventoryItem{Key: c.Key(), Project: c.Project, Service: c.Service, Name: c.Name, Label: c.OnFailover()})
		fmt.Fprintf(&print, "%s=%s:%v;", c.ID, c.Name, c.Running)
	}
	added, err := e.Store.SyncInventory(ctx, items, e.Now())
	if err != nil {
		return nil, err
	}
	if len(added) > 0 {
		e.Log.Info("new containers found", "keys", added)
	}
	e.mu.Lock()
	changed := print.String() != e.lastPrint
	e.lastPrint = print.String()
	e.mu.Unlock()
	if changed {
		e.changed()
	}
	return cs, nil
}

// Sync refreshes the inventory from Docker.
func (e *Executor) Sync(ctx context.Context) error {
	_, err := e.list(ctx)
	return err
}

// Stop stops every container whose effective policy is disable. It is safe to
// call again for the same failover: the open run is reused, and containers
// already recorded keep their original was_running. trigger is "auto" or
// "manual".
func (e *Executor) Stop(ctx context.Context, trigger string, toASN int) error {
	defer e.changed()
	cs, err := e.list(ctx)
	if err != nil {
		return err
	}
	rows, err := e.Store.Containers(ctx)
	if err != nil {
		return err
	}
	effective := map[string]string{}
	for _, r := range rows {
		effective[r.Key] = r.Effective
	}

	run, err := e.Store.OpenRun(ctx)
	if err != nil {
		return err
	}
	if run == nil {
		if run, err = e.Store.CreateRun(ctx, trigger, toASN, e.Now()); err != nil {
			return err
		}
	}

	var targets []docker.Container
	for _, c := range cs {
		if effective[c.Key()] != store.Disable {
			continue
		}
		if err := e.Store.AddRunAction(ctx, run.ID, store.RunAction{
			ContainerID: c.ID, Key: c.Key(), Name: c.Name, WasRunning: c.Running,
		}); err != nil {
			return err
		}
		if c.Running {
			targets = append(targets, c)
		}
	}
	e.Log.Info("failover: stopping containers", "run", run.ID, "count", len(targets))

	layers := Layers(targets)
	slices.Reverse(layers) // dependents first
	var errs []error
	for _, layer := range layers {
		errs = append(errs, e.each(ctx, layer, func(i int) error {
			c := targets[i]
			err := e.Docker.Stop(ctx, c.ID, e.StopTimeout)
			e.record(ctx, e.Store.MarkStopped, run.ID, c.ID, c.Name, err)
			return err
		})...)
	}
	return errors.Join(errs...)
}

// Start starts the containers the open run stopped, if they were running
// before, then closes the run. Without an open run it does nothing.
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
	cs, err := e.list(ctx)
	if err != nil {
		return err
	}
	byID := map[string]docker.Container{}
	byKey := map[string][]docker.Container{}
	for _, c := range cs {
		byID[c.ID] = c
		byKey[c.Key()] = append(byKey[c.Key()], c)
	}

	var targets []docker.Container
	var recorded []string // the run_actions container ID for each target
	var errs []error
	seen := map[string]bool{}
	for _, a := range actions {
		if !a.WasRunning || a.Started {
			continue
		}
		// Prefer the exact container; fall back to its key if it was re-created.
		found := []docker.Container{}
		if c, ok := byID[a.ContainerID]; ok {
			found = append(found, c)
		} else {
			found = byKey[a.Key]
		}
		if len(found) == 0 {
			err := fmt.Errorf("%s no longer exists", a.Name)
			errs = append(errs, err)
			e.report(e.Store.MarkStarted(ctx, run.ID, a.ContainerID, err.Error(), e.Now()))
			continue
		}
		for _, c := range found {
			if !seen[c.ID] {
				seen[c.ID] = true
				targets = append(targets, c)
				recorded = append(recorded, a.ContainerID)
			}
		}
	}
	e.Log.Info("failback: starting containers", "run", run.ID, "count", len(targets))

	layers := Layers(targets)
	for n, layer := range layers {
		errs = append(errs, e.each(ctx, layer, func(i int) error {
			c := targets[i]
			err := e.Docker.Start(ctx, c.ID)
			e.record(ctx, e.Store.MarkStarted, run.ID, recorded[i], c.Name, err)
			return err
		})...)
		if n < len(layers)-1 {
			e.waitHealthy(ctx, targets, layer)
		}
	}

	status := "completed"
	if len(errs) > 0 {
		status = "failed"
	}
	e.report(e.Store.CloseRun(ctx, run.ID, status, e.Now()))
	return errors.Join(errs...)
}

func (e *Executor) record(ctx context.Context, mark func(context.Context, int64, string, string, time.Time) error,
	runID int64, containerID, name string, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
		e.Log.Error("container action failed", "container", name, "err", err)
	} else {
		e.Log.Info("container action done", "container", name)
	}
	e.report(mark(ctx, runID, containerID, msg, e.Now()))
}

// each runs fn for every index, at most e.Parallel at a time.
func (e *Executor) each(_ context.Context, idx []int, fn func(int) error) []error {
	sem := make(chan struct{}, max(1, e.Parallel))
	var mu sync.Mutex
	var errs []error
	var wg sync.WaitGroup
	for _, i := range idx {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			if err := fn(i); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return errs
}

// waitHealthy waits until every container with a healthcheck reports healthy,
// up to HealthWait. Containers without a healthcheck count as ready.
func (e *Executor) waitHealthy(ctx context.Context, cs []docker.Container, idx []int) {
	deadline := time.Now().Add(e.HealthWait)
	for _, i := range idx {
		c := cs[i]
		for {
			running, health, err := e.Docker.Inspect(ctx, c.ID)
			if err != nil || !running || health == "" || health == "healthy" {
				break
			}
			if time.Now().After(deadline) {
				e.Log.Warn("healthcheck wait timed out; starting dependents anyway", "container", c.Name, "health", health)
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
	rows, err := e.Store.Containers(ctx)
	if err != nil {
		return err
	}
	effective := map[string]string{}
	for _, r := range rows {
		effective[r.Key] = r.Effective
	}
	for _, a := range actions {
		if !a.WasRunning || !a.Stopped || a.Started || effective[a.Key] == store.Disable {
			continue
		}
		err := e.Docker.Start(ctx, a.ContainerID)
		e.record(ctx, e.Store.MarkStarted, run.ID, a.ContainerID, a.Name, err)
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

// Layers groups container indices by Compose dependency depth: layer 0 depends
// on nothing in the set, layer 1 only on layer 0, and so on. Dependencies
// outside the set, and cycles, are ignored.
func Layers(cs []docker.Container) [][]int {
	type node struct {
		project, service string
	}
	bySvc := map[node][]int{}
	for i, c := range cs {
		if c.Service != "" {
			n := node{c.Project, c.Service}
			bySvc[n] = append(bySvc[n], i)
		}
	}
	depth := make([]int, len(cs))
	state := make([]int, len(cs)) // 0 new, 1 visiting, 2 done
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
		for _, svc := range cs[i].DependsOn() {
			for _, j := range bySvc[node{cs[i].Project, svc}] {
				if dd := visit(j); dd >= 0 {
					d = max(d, dd+1)
				}
			}
		}
		state[i], depth[i] = 2, d
		return d
	}
	var layers [][]int
	for i := range cs {
		d := visit(i)
		for len(layers) <= d {
			layers = append(layers, nil)
		}
		layers[d] = append(layers[d], i)
	}
	return layers
}
