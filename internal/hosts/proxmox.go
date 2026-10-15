// SPDX-License-Identifier: AGPL-3.0-or-later

package hosts

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/proxmox"
)

// KindLXC is an LXC container on Proxmox VE.
const KindLXC = "lxc"

// Proxmox tags that work like the drawbridge.on-failover label.
const (
	TagStop = "drawbridge-stop"
	TagKeep = "drawbridge-keep"
)

// haStartWait bounds how long a start through HA may take.
const haStartWait = 2 * time.Minute

// ProxmoxAPI is what the Proxmox runtime needs from the API.
type ProxmoxAPI interface {
	Guests(ctx context.Context) ([]proxmox.Guest, error)
	StartupOrder(ctx context.Context, g proxmox.Guest) (int, error)
	HAManaged(ctx context.Context) (map[string]bool, error)
	Shutdown(ctx context.Context, g proxmox.Guest, timeout time.Duration) error
	Start(ctx context.Context, g proxmox.Guest) error
	SetHAState(ctx context.Context, g proxmox.Guest, state string, wait time.Duration) error
}

// Proxmox is the runtime of a Proxmox VE cluster's LXC containers, grouped by
// pool. Within a pool, the guests' start order becomes their dependencies.
// Guests under HA are stopped and started through HA.
type Proxmox struct {
	API  ProxmoxAPI
	Self string // the agent's hostname: a guest of that name is the agent's own and left out
}

// haSID is a container's HA resource ID.
func haSID(vmid int) string { return "ct:" + strconv.Itoa(vmid) }

// List returns the cluster's LXC containers.
func (p Proxmox) List(ctx context.Context) ([]Workload, error) {
	gs, err := p.API.Guests(ctx)
	if err != nil {
		return nil, err
	}
	gs = slices.DeleteFunc(gs, func(g proxmox.Guest) bool { return p.Self != "" && g.Name == p.Self })
	order := map[int]int{}
	for _, g := range gs {
		if g.Pool == "" {
			continue // order only links guests of a pool
		}
		if order[g.VMID], err = p.API.StartupOrder(ctx, g); err != nil {
			return nil, err
		}
	}
	out := make([]Workload, 0, len(gs))
	for _, g := range gs {
		out = append(out, fromGuest(g, gs, order))
	}
	return out, nil
}

// fromGuest turns a guest into a workload. A guest depends on the guests of
// its pool with a lower start order; guests without one start last.
func fromGuest(g proxmox.Guest, all []proxmox.Guest, order map[int]int) Workload {
	name := g.Name
	if name == "" {
		name = "CT " + strconv.Itoa(g.VMID)
	}
	w := Workload{ID: g.ID, Name: name, Kind: KindLXC, Running: g.Status == "running"}
	if g.Pool != "" {
		w.Group, w.Member = g.Pool, strconv.Itoa(g.VMID)
		mine := order[g.VMID]
		for _, o := range all {
			theirs := order[o.VMID]
			if o.Pool == g.Pool && o.VMID != g.VMID && theirs > 0 && (mine == 0 || theirs < mine) {
				w.DependsOn = append(w.DependsOn, strconv.Itoa(o.VMID))
			}
		}
	}
	tags := g.TagList()
	switch {
	case slices.Contains(tags, TagStop):
		w.Label = "disable"
	case slices.Contains(tags, TagKeep):
		w.Label = "keep"
	}
	return w
}

func (p Proxmox) guest(ctx context.Context, id string) (proxmox.Guest, error) {
	gs, err := p.API.Guests(ctx)
	if err != nil {
		return proxmox.Guest{}, err
	}
	i := slices.IndexFunc(gs, func(g proxmox.Guest) bool { return g.ID == id })
	if i < 0 {
		return proxmox.Guest{}, fmt.Errorf("no LXC container %s", id)
	}
	return gs[i], nil
}

// managed reports whether HA manages a container. A token that may not read
// HA (it needs Sys.Audit on /) sees no HA, and containers are stopped
// directly.
func (p Proxmox) managed(ctx context.Context, g proxmox.Guest) bool {
	ha, err := p.API.HAManaged(ctx)
	return err == nil && ha[haSID(g.VMID)]
}

// Inspect reports whether a container runs; containers have no health.
func (p Proxmox) Inspect(ctx context.Context, w Workload) (bool, string, error) {
	g, err := p.guest(ctx, w.ID)
	return g.Status == "running", "", err
}

type proxmoxRestore struct {
	HA bool `json:"ha,omitempty"`
}

// Stop shuts a container down, forcing it after timeout; through HA when it
// is HA-managed, or HA would start it again.
func (p Proxmox) Stop(ctx context.Context, w Workload, timeout time.Duration) ([]byte, error) {
	g, err := p.guest(ctx, w.ID)
	if err != nil {
		return nil, err
	}
	if p.managed(ctx, g) {
		if err := p.API.SetHAState(ctx, g, "stopped", timeout+30*time.Second); err != nil {
			return nil, err
		}
		return json.Marshal(proxmoxRestore{HA: true})
	}
	return nil, p.API.Shutdown(ctx, g, timeout)
}

// Start starts a container, through HA if it was stopped through HA or is
// HA-managed now.
func (p Proxmox) Start(ctx context.Context, w Workload, restore []byte) error {
	g, err := p.guest(ctx, w.ID)
	if err != nil {
		return err
	}
	var r proxmoxRestore
	if len(restore) > 0 {
		if err := json.Unmarshal(restore, &r); err != nil {
			return fmt.Errorf("restore data: %w", err)
		}
	}
	if !r.HA {
		r.HA = p.managed(ctx, g)
	}
	if r.HA {
		return p.API.SetHAState(ctx, g, "started", haStartWait)
	}
	return p.API.Start(ctx, g)
}

// Stats reads a running container's network counters, which count from its
// start like Docker's.
func (p Proxmox) Stats(ctx context.Context, w Workload) (int64, int64, bool, error) {
	g, err := p.guest(ctx, w.ID)
	if err != nil {
		return 0, 0, false, err
	}
	return g.NetIn, g.NetOut, g.Status == "running", nil
}
