// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"context"
	"time"
)

// EventSource streams Docker container events.
type EventSource interface {
	Events(ctx context.Context, fn func(action, id string)) error
}

// WatchInventory syncs the inventory every interval, and shortly after Docker
// reports a container being created, destroyed or renamed.
func (e *Executor) WatchInventory(ctx context.Context, src EventSource, every time.Duration) {
	changed := make(chan struct{}, 1)
	go func() {
		for ctx.Err() == nil {
			err := src.Events(ctx, func(action, _ string) {
				switch action {
				case "create", "destroy", "rename":
					select {
					case changed <- struct{}{}:
					default:
					}
				}
			})
			if ctx.Err() != nil {
				return
			}
			e.Log.Warn("docker event stream ended; reconnecting", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()

	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if err := e.Sync(ctx); err != nil {
			e.Log.Warn("inventory sync failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-changed:
			// Let a burst of events (compose up) settle before syncing.
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
}
