// SPDX-License-Identifier: AGPL-3.0-or-later

package hosts

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/docker"
)

// KindService is a Docker Swarm service.
const KindService = "service"

// Swarm placement and labels Drawbridge sets on a service it stops.
const (
	// StoppedConstraint is the placement constraint that takes every task of
	// a global service off every node: no node has this label.
	StoppedConstraint = "node.labels.drawbridge-stopped==true"
	// LabelReplicas keeps a stopped service's replica count on the service
	// itself, so it can be started again even without the stored run.
	LabelReplicas = "drawbridge.replicas"
)

// SwarmAPI is what the Swarm runtime needs from Docker.
type SwarmAPI interface {
	DockerAPI
	Swarm(ctx context.Context) (docker.SwarmInfo, error)
	Services(ctx context.Context) ([]docker.Service, error)
	ServiceSpec(ctx context.Context, id string) (uint64, map[string]any, error)
	UpdateService(ctx context.Context, id string, version uint64, spec map[string]any) error
}

// Swarm is the runtime of a Docker Swarm manager node: services, grouped by
// stack, and the node's own standalone containers. Tasks of services are not
// listed as containers. A stopped replicated service is scaled to 0; a
// stopped global service gets StoppedConstraint.
type Swarm struct {
	Docker SwarmAPI
	SelfID string // this container's ID or ID prefix (its hostname)
}

// restoreData is what starting a stopped service again needs.
type restoreData struct {
	Replicas *uint64 `json:"replicas,omitempty"`
	Global   bool    `json:"global,omitempty"`
}

// List returns the services and the standalone containers, without the
// agent's own container and service.
func (s Swarm) List(ctx context.Context) ([]Workload, error) {
	cs, err := s.Docker.List(ctx)
	if err != nil {
		return nil, err
	}
	local := Local{SelfID: s.SelfID}
	selfService := ""
	var out []Workload
	for _, c := range cs {
		if local.isSelf(c) {
			selfService = c.Labels[docker.LabelSwarmTask]
			continue
		}
		if c.Labels[docker.LabelSwarmTask] == "" {
			out = append(out, FromContainer(c))
		}
	}
	ss, err := s.Docker.Services(ctx)
	if err != nil {
		return nil, err
	}
	for _, svc := range ss {
		if svc.ID != selfService && svc.Labels[docker.LabelSelf] != "true" {
			out = append(out, fromService(svc))
		}
	}
	return out, nil
}

func fromService(svc docker.Service) Workload {
	group := svc.Labels[docker.LabelStack]
	member := ""
	if group != "" {
		member = strings.TrimPrefix(svc.Name, group+"_")
	}
	return Workload{
		ID: svc.ID, Name: svc.Name, Kind: KindService, Group: group, Member: member,
		Running: svc.Running > 0, Label: docker.Container{Labels: svc.Labels}.OnFailover(),
	}
}

func (s Swarm) service(ctx context.Context, id string) (docker.Service, error) {
	ss, err := s.Docker.Services(ctx)
	if err != nil {
		return docker.Service{}, err
	}
	i := slices.IndexFunc(ss, func(svc docker.Service) bool { return svc.ID == id })
	if i < 0 {
		return docker.Service{}, fmt.Errorf("no service %s", id)
	}
	return ss[i], nil
}

// Inspect reports whether a service runs: health is "starting" until all
// its tasks run, then "healthy".
func (s Swarm) Inspect(ctx context.Context, w Workload) (bool, string, error) {
	if w.Kind != KindService {
		return Local{Docker: s.Docker}.Inspect(ctx, w)
	}
	svc, err := s.service(ctx, w.ID)
	if err != nil {
		return false, "", err
	}
	if svc.Running < svc.Desired {
		return svc.Running > 0, "starting", nil
	}
	return svc.Running > 0, "healthy", nil
}

// update changes a service's spec, reading it again when Docker refuses an
// update made from an older version.
func (s Swarm) update(ctx context.Context, id string, change func(spec map[string]any) error) error {
	var err error
	for range 3 {
		var version uint64
		var spec map[string]any
		if version, spec, err = s.Docker.ServiceSpec(ctx, id); err != nil {
			return err
		}
		if err = change(spec); err != nil {
			return err
		}
		if err = s.Docker.UpdateService(ctx, id, version, spec); err == nil || !strings.Contains(err.Error(), "out of sequence") {
			return err
		}
	}
	return err
}

// object returns m[k] as a map, creating it if needed.
func object(m map[string]any, k string) map[string]any {
	o, ok := m[k].(map[string]any)
	if !ok {
		o = map[string]any{}
		m[k] = o
	}
	return o
}

// Stop scales a replicated service to 0, or keeps a global one off every
// node; a container is stopped as on any Docker host.
func (s Swarm) Stop(ctx context.Context, w Workload, timeout time.Duration) ([]byte, error) {
	if w.Kind != KindService {
		return Local{Docker: s.Docker}.Stop(ctx, w, timeout)
	}
	var r restoreData
	err := s.update(ctx, w.ID, func(spec map[string]any) error {
		mode := object(spec, "Mode")
		if _, global := mode["Global"]; global {
			r = restoreData{Global: true}
			placement := object(object(spec, "TaskTemplate"), "Placement")
			cs, _ := placement["Constraints"].([]any)
			if !slices.Contains(cs, any(StoppedConstraint)) {
				placement["Constraints"] = append(cs, StoppedConstraint)
			}
			return nil
		}
		rep := object(mode, "Replicated")
		n := uint64(1)
		if f, ok := rep["Replicas"].(float64); ok {
			n = uint64(f)
		}
		r = restoreData{Replicas: &n}
		rep["Replicas"] = 0
		object(spec, "Labels")[LabelReplicas] = strconv.FormatUint(n, 10)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(r)
}

// Start undoes Stop: the replica count comes back (from restore, or else from
// the service's LabelReplicas, or 1), or the constraint is removed.
func (s Swarm) Start(ctx context.Context, w Workload, restore []byte) error {
	if w.Kind != KindService {
		return Local{Docker: s.Docker}.Start(ctx, w, restore)
	}
	var r restoreData
	if len(restore) > 0 {
		if err := json.Unmarshal(restore, &r); err != nil {
			return fmt.Errorf("restore data: %w", err)
		}
	}
	return s.update(ctx, w.ID, func(spec map[string]any) error {
		mode := object(spec, "Mode")
		labels := object(spec, "Labels")
		if _, global := mode["Global"]; global {
			placement := object(object(spec, "TaskTemplate"), "Placement")
			cs, _ := placement["Constraints"].([]any)
			placement["Constraints"] = slices.DeleteFunc(cs, func(c any) bool { return c == StoppedConstraint })
			return nil
		}
		n := uint64(1)
		switch {
		case r.Replicas != nil:
			n = *r.Replicas
		case labels[LabelReplicas] != nil:
			if v, err := strconv.ParseUint(fmt.Sprint(labels[LabelReplicas]), 10, 64); err == nil {
				n = v
			}
		}
		object(mode, "Replicated")["Replicas"] = n
		delete(labels, LabelReplicas)
		return nil
	})
}

// Stats reads a container's counters; services have none (their tasks run
// on several nodes).
func (s Swarm) Stats(ctx context.Context, w Workload) (int64, int64, bool, error) {
	if w.Kind != KindService {
		return Local{Docker: s.Docker}.Stats(ctx, w)
	}
	return 0, 0, false, nil
}
