// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"context"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/store"
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
	return e.checkDataAlerts(ctx, outage)
}

// checkDataAlerts records a data_alert event the first time the traffic of
// the open outage, or of this calendar month's outages, passes its limit.
func (e *Executor) checkDataAlerts(ctx context.Context, outage int64) error {
	var limits store.DataAlerts
	if _, err := e.Store.GetSetting(ctx, store.SettingDataAlerts, &limits); err != nil {
		return err
	}
	if limits.OutageBytes <= 0 && limits.MonthBytes <= 0 {
		return nil
	}
	var sent store.DataAlertsSent
	if _, err := e.Store.GetSetting(ctx, store.SettingDataAlertsSent, &sent); err != nil {
		return err
	}
	now := e.Now()
	was := sent
	if limits.OutageBytes > 0 && sent.Outage != outage {
		used, err := e.Store.OutageTotal(ctx, outage)
		if err != nil {
			return err
		}
		if used >= limits.OutageBytes {
			e.dataAlert(ctx, "outage", used, limits.OutageBytes)
			sent.Outage = outage
		}
	}
	month := now.Format("2006-01")
	if limits.MonthBytes > 0 && sent.Month != month {
		start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		used, err := e.Store.TotalSince(ctx, start)
		if err != nil {
			return err
		}
		if used >= limits.MonthBytes {
			e.dataAlert(ctx, "month", used, limits.MonthBytes)
			sent.Month = month
		}
	}
	if sent == was {
		return nil
	}
	return e.Store.SetSetting(ctx, store.SettingDataAlertsSent, sent)
}

func (e *Executor) dataAlert(ctx context.Context, scope string, used, limit int64) {
	e.Log.Warn("data alert", "scope", scope, "used", used, "limit", limit)
	e.report(e.Store.AddEvent(ctx, e.Now(), "data_alert", map[string]any{"scope": scope, "used": used, "limit": limit}))
}
