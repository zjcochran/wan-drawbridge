// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"context"
	"time"
)

// StatsSource reads a container's network counters.
type StatsSource interface {
	Stats(ctx context.Context, id string) (rx, tx int64, ok bool, err error)
}

// WatchUsage samples the network counters of every running container each
// interval while an outage lasts, adding the traffic to the outage.
func (e *Executor) WatchUsage(ctx context.Context, src StatsSource, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if err := e.SampleUsage(ctx, src); err != nil {
			e.Log.Warn("usage sample failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// SampleUsage takes one sample, when an outage lasts. A container whose
// counters cannot be read is skipped until the next sample.
func (e *Executor) SampleUsage(ctx context.Context, src StatsSource) error {
	outage, err := e.Store.OpenOutage(ctx)
	if err != nil || outage == 0 {
		return err
	}
	cs, err := e.Docker.List(ctx)
	if err != nil {
		return err
	}
	for _, c := range cs {
		if !c.Running {
			continue
		}
		rx, tx, ok, err := src.Stats(ctx, c.ID)
		if err != nil {
			e.Log.Debug("container stats failed", "container", c.Key(), "err", err)
			continue
		}
		if !ok {
			continue
		}
		if err := e.Store.RecordUsage(ctx, outage, c.Key(), c.ID, rx, tx, e.Now()); err != nil {
			return err
		}
	}
	return nil
}
