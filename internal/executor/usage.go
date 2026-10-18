// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"context"
	"strconv"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/hosts"
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

// SampleUsage takes one sample on every host that can be reached: into the
// outage while one lasts, or else into the month of the primary ISP while
// traffic is on it and it is metered with a monthly limit. A container whose
// counters cannot be read is skipped until the next sample.
func (e *Executor) SampleUsage(ctx context.Context) error {
	outage, err := e.Store.OpenOutage(ctx)
	if err != nil {
		return err
	}
	if outage != 0 {
		if err := e.sample(ctx, func(w hosts.Workload, rx, tx int64) error {
			return e.Store.RecordUsage(ctx, outage, w.Host, w.Key(), w.ID, rx, tx, e.Now())
		}); err != nil {
			return err
		}
		return e.checkDataAlerts(ctx, outage)
	}
	isp, limit, err := e.Store.PrimaryMeter(ctx)
	if err != nil || isp == 0 {
		return err
	}
	month := e.Now().Format("2006-01")
	if err := e.sample(ctx, func(w hosts.Workload, rx, tx int64) error {
		return e.Store.RecordISPUsage(ctx, isp, month, w.Host, w.Key(), w.ID, rx, tx, e.Now())
	}); err != nil {
		return err
	}
	return e.checkPrimaryLimit(ctx, isp, month, limit)
}

// sample reads the counters of every running container and passes them on.
func (e *Executor) sample(ctx context.Context, record func(w hosts.Workload, rx, tx int64) error) error {
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
			if err := record(w, rx, tx); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkPrimaryLimit records a data_alert the first time this month's
// traffic on the metered primary passes its monthly limit.
func (e *Executor) checkPrimaryLimit(ctx context.Context, isp int, month string, limit int64) error {
	var sent store.DataAlertsSent
	if _, err := e.Store.GetSetting(ctx, store.SettingDataAlertsSent, &sent); err != nil {
		return err
	}
	key := strconv.Itoa(isp)
	if sent.Months[key] == month {
		return nil
	}
	used, err := e.Store.ISPMonthUsage(ctx, isp, month)
	if err != nil || used < limit {
		return err
	}
	e.dataAlert(ctx, "month", isp, used, limit, true)
	if sent.Months == nil {
		sent.Months = map[string]string{}
	}
	sent.Months[key] = month
	return e.Store.SetSetting(ctx, store.SettingDataAlertsSent, sent)
}

// checkDataAlerts records a data_alert event the first time the traffic of
// the open outage, or of this calendar month's outages on the same ISP,
// passes that ISP's limit.
func (e *Executor) checkDataAlerts(ctx context.Context, outage int64) error {
	isp, limits, err := e.Store.OutageLimits(ctx, outage)
	if err != nil || isp == 0 || (limits.OutageBytes <= 0 && limits.MonthBytes <= 0) {
		return err
	}
	var sent store.DataAlertsSent
	if _, err := e.Store.GetSetting(ctx, store.SettingDataAlertsSent, &sent); err != nil {
		return err
	}
	if sent.Months == nil {
		sent.Months = map[string]string{}
	}
	changed := false
	if limits.OutageBytes > 0 && sent.Outage != outage {
		used, err := e.Store.OutageTotal(ctx, outage)
		if err != nil {
			return err
		}
		if used >= limits.OutageBytes {
			e.dataAlert(ctx, "outage", isp, used, limits.OutageBytes, false)
			sent.Outage, changed = outage, true
		}
	}
	now := e.Now()
	month, key := now.Format("2006-01"), strconv.Itoa(isp)
	if limits.MonthBytes > 0 && sent.Months[key] != month {
		start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		used, err := e.Store.ISPTotalSince(ctx, isp, start)
		if err != nil {
			return err
		}
		if used >= limits.MonthBytes {
			e.dataAlert(ctx, "month", isp, used, limits.MonthBytes, false)
			sent.Months[key], changed = month, true
		}
	}
	if !changed {
		return nil
	}
	return e.Store.SetSetting(ctx, store.SettingDataAlertsSent, sent)
}

// dataAlert records a data_alert; primary marks the primary ISP's month,
// counted all month rather than during outages.
func (e *Executor) dataAlert(ctx context.Context, scope string, isp int, used, limit int64, primary bool) {
	e.Log.Warn("data alert", "scope", scope, "isp", isp, "used", used, "limit", limit, "primary", primary)
	payload := map[string]any{"scope": scope, "isp": isp, "used": used, "limit": limit}
	if primary {
		payload["primary"] = true
	}
	e.report(e.Store.AddEvent(ctx, e.Now(), "data_alert", payload))
}
