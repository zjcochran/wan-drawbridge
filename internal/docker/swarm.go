// SPDX-License-Identifier: AGPL-3.0-or-later

package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
)

// Swarm labels.
const (
	LabelStack       = "com.docker.stack.namespace"
	LabelSwarmTask   = "com.docker.swarm.service.id"
	LabelSwarmTaskOf = "com.docker.swarm.service.name"
)

// SwarmInfo is this engine's place in a swarm.
type SwarmInfo struct {
	NodeID  string
	State   string // LocalNodeState: "inactive", "active", ...
	Manager bool   // ControlAvailable: this node can manage the swarm
}

// Swarm reads the engine's swarm state.
func (c *Client) Swarm(ctx context.Context) (SwarmInfo, error) {
	var info struct {
		Swarm struct {
			NodeID           string
			LocalNodeState   string
			ControlAvailable bool
		}
	}
	err := c.getJSON(ctx, "/info", &info)
	return SwarmInfo{NodeID: info.Swarm.NodeID, State: info.Swarm.LocalNodeState, Manager: info.Swarm.ControlAvailable}, err
}

// Service is a swarm service with its task counts.
type Service struct {
	ID       string
	Name     string
	Labels   map[string]string // the service's own labels
	Global   bool
	Replicas uint64 // wanted replicas of a replicated service
	Running  uint64 // tasks running now
	Desired  uint64 // tasks that should be running
}

// Services lists the swarm's services with their task counts. It needs a
// manager node.
func (c *Client) Services(ctx context.Context) ([]Service, error) {
	var raw []struct {
		ID   string
		Spec struct {
			Name   string
			Labels map[string]string
			Mode   struct {
				Replicated *struct{ Replicas uint64 }
				Global     *struct{}
			}
		}
		ServiceStatus struct {
			RunningTasks uint64
			DesiredTasks uint64
		}
	}
	if err := c.getJSON(ctx, "/services?status=true", &raw); err != nil {
		return nil, err
	}
	out := make([]Service, 0, len(raw))
	for _, r := range raw {
		s := Service{ID: r.ID, Name: r.Spec.Name, Labels: r.Spec.Labels, Global: r.Spec.Mode.Global != nil,
			Running: r.ServiceStatus.RunningTasks, Desired: r.ServiceStatus.DesiredTasks}
		if r.Spec.Mode.Replicated != nil {
			s.Replicas = r.Spec.Mode.Replicated.Replicas
		}
		out = append(out, s)
	}
	return out, nil
}

// ServiceSpec reads a service's version and full spec, to change it with
// UpdateService. The spec is kept as generic JSON so nothing in it is lost.
func (c *Client) ServiceSpec(ctx context.Context, id string) (uint64, map[string]any, error) {
	var raw struct {
		Version struct{ Index uint64 }
		Spec    map[string]any
	}
	err := c.getJSON(ctx, "/services/"+url.PathEscape(id), &raw)
	return raw.Version.Index, raw.Spec, err
}

// UpdateService replaces a service's spec. version must be the one the spec
// was read at; Docker refuses an update made from an older one.
func (c *Client) UpdateService(ctx context.Context, id string, version uint64, spec map[string]any) error {
	body, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/services/"+url.PathEscape(id)+"/update?version="+strconv.FormatUint(version, 10), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	return nil
}
