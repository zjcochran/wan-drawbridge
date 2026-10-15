// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"context"
	"time"
)

// WatchUsage samples the network counters of every running container each
// interval while an outage lasts, adding the traffic to the outage.
func (e *Executor) WatchUsage(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if err := e.SampleUsage(ctx); err != nil {
			e.Log.Warn("usage sample failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// SampleUsage takes one sample on every host that can be reached, when an
// outage lasts. A container whose counters cannot be read is skipped until
// the next sample.
func (e *Executor) SampleUsage(ctx context.Context) error {
	outage, err := e.Store.OpenOutage(ctx)
	if err != nil || outage == 0 {
		return err
	}
	for _, l := range e.Hosts.List(ctx, e.ListTimeout) {
		if l.Err != nil {
			continue
		}
		for _, w := range l.Workloads {
			if !w.Running {
				continue
			}
			rx, tx, ok, err := l.Host.Runtime.Stats(ctx, w)
			if err != nil {
				e.Log.Debug("container stats failed", "host", l.Host.Name, "container", w.Key(), "err", err)
				continue
			}
			if !ok {
				continue
			}
			if err := e.Store.RecordUsage(ctx, outage, w.Host, w.Key(), w.ID, rx, tx, e.Now()); err != nil {
				return err
			}
		}
	}
	return nil
}
