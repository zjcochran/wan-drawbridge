// SPDX-License-Identifier: AGPL-3.0-or-later

// Package hosts is the places Drawbridge controls: each host has a Runtime
// that lists, stops and starts its workloads. Host 1 is the local Docker
// socket.
package hosts

import (
	"context"
	"slices"
	"sync"
	"time"
)

// Workload kinds.
const (
	KindContainer = "container"
)

// Workload is something a host can stop and start: a Docker container today.
// Agents send workloads as JSON, so the field names are part of the agent
// protocol.
type Workload struct {
	Host      int64    `json:"-"` // set by the manager
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Group     string   `json:"group,omitempty"`  // the Compose project, "" for a standalone container
	Member    string   `json:"member,omitempty"` // the Compose service, "" for a standalone container
	Running   bool     `json:"running"`
	Label     string   `json:"label,omitempty"`      // from the drawbridge.on-failover label: "", "disable" or "keep"
	DependsOn []string `json:"depends_on,omitempty"` // members of the same group it depends on
}

// Key identifies a workload on its host across re-creation: group/member for
// Compose containers, the name otherwise.
func (w Workload) Key() string {
	if w.Group != "" && w.Member != "" {
		return w.Group + "/" + w.Member
	}
	return w.Name
}

// Runtime controls one host's workloads.
type Runtime interface {
	List(ctx context.Context) ([]Workload, error)
	// Inspect reports whether w runs, and its healthcheck status: "" without
	// one, else "starting", "healthy" or "unhealthy".
	Inspect(ctx context.Context, w Workload) (running bool, health string, err error)
	// Stop returns what Start needs to undo it, nil when nothing is needed.
	Stop(ctx context.Context, w Workload, timeout time.Duration) (restore []byte, err error)
	Start(ctx context.Context, w Workload, restore []byte) error
	// Stats reads w's network counters; ok is false when it has none.
	Stats(ctx context.Context, w Workload) (rx, tx int64, ok bool, err error)
}

// Host is one host with its runtime.
type Host struct {
	ID      int64
	Name    string
	Runtime Runtime
}

// Registry holds the hosts Drawbridge can reach. It is safe for concurrent use.
type Registry struct {
	mu    sync.RWMutex
	hosts map[int64]Host
}

// NewRegistry returns a registry holding hs.
func NewRegistry(hs ...Host) *Registry {
	r := &Registry{hosts: map[int64]Host{}}
	for _, h := range hs {
		r.Set(h)
	}
	return r
}

// Set adds or replaces a host.
func (r *Registry) Set(h Host) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hosts[h.ID] = h
}

// Remove drops a host.
func (r *Registry) Remove(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.hosts, id)
}

// Get returns a host by ID.
func (r *Registry) Get(id int64) (Host, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.hosts[id]
	return h, ok
}

// All returns every host, by ID.
func (r *Registry) All() []Host {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Host, 0, len(r.hosts))
	for _, h := range r.hosts {
		out = append(out, h)
	}
	slices.SortFunc(out, func(a, b Host) int { return int(a.ID - b.ID) })
	return out
}

// Listing is one host's workloads, or why they could not be listed.
type Listing struct {
	Host      Host
	Workloads []Workload
	Err       error
}

// List lists every host at once, each with its own timeout, in host order.
// Each workload gets its host's ID.
func (r *Registry) List(ctx context.Context, timeout time.Duration) []Listing {
	hs := r.All()
	out := make([]Listing, len(hs))
	var wg sync.WaitGroup
	for i, h := range hs {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			ws, err := h.Runtime.List(ctx)
			for j := range ws {
				ws[j].Host = h.ID
			}
			out[i] = Listing{Host: h, Workloads: ws, Err: err}
		})
	}
	wg.Wait()
	return out
}
