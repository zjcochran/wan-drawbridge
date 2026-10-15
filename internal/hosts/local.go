// SPDX-License-Identifier: AGPL-3.0-or-later

package hosts

import (
	"context"
	"strings"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/docker"
)

// DockerAPI is the part of the Docker client the local runtime uses.
type DockerAPI interface {
	List(ctx context.Context) ([]docker.Container, error)
	Inspect(ctx context.Context, id string) (running bool, health docker.Health, err error)
	Stop(ctx context.Context, id string, timeout time.Duration) error
	Start(ctx context.Context, id string) error
	Stats(ctx context.Context, id string) (rx, tx int64, ok bool, err error)
}

// Local is the runtime of a Docker engine Drawbridge reaches directly: the
// local socket. It never lists Drawbridge's own container.
type Local struct {
	Docker DockerAPI
	SelfID string // this container's ID or ID prefix (its hostname)
}

// isSelf reports whether c is Drawbridge's own container.
func (l Local) isSelf(c docker.Container) bool {
	if c.Labels[docker.LabelSelf] == "true" {
		return true
	}
	return len(l.SelfID) >= 12 && strings.HasPrefix(c.ID, l.SelfID)
}

// List returns the containers, without Drawbridge itself.
func (l Local) List(ctx context.Context) ([]Workload, error) {
	cs, err := l.Docker.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Workload, 0, len(cs))
	for _, c := range cs {
		if !l.isSelf(c) {
			out = append(out, FromContainer(c))
		}
	}
	return out, nil
}

// FromContainer turns a Docker container into a workload.
func FromContainer(c docker.Container) Workload {
	return Workload{
		ID: c.ID, Name: c.Name, Kind: KindContainer, Group: c.Project, Member: c.Service,
		Running: c.Running, Label: c.OnFailover(), DependsOn: c.DependsOn(),
	}
}

// Inspect reports the container's state and health.
func (l Local) Inspect(ctx context.Context, w Workload) (bool, string, error) {
	running, health, err := l.Docker.Inspect(ctx, w.ID)
	return running, string(health), err
}

// Stop stops the container; starting it again needs nothing more.
func (l Local) Stop(ctx context.Context, w Workload, timeout time.Duration) ([]byte, error) {
	return nil, l.Docker.Stop(ctx, w.ID, timeout)
}

// Start starts the container.
func (l Local) Start(ctx context.Context, w Workload, _ []byte) error {
	return l.Docker.Start(ctx, w.ID)
}

// Stats reads the container's network counters.
func (l Local) Stats(ctx context.Context, w Workload) (int64, int64, bool, error) {
	return l.Docker.Stats(ctx, w.ID)
}
