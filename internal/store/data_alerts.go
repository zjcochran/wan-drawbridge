// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"time"
)

// Setting keys for data alerts.
const (
	SettingDataAlerts     = "data_alerts"      // DataAlerts
	SettingDataAlertsSent = "data_alerts_sent" // DataAlertsSent
)

// DataAlerts are the limits on the traffic of containers during outages,
// in bytes. 0 turns a limit off.
type DataAlerts struct {
	OutageBytes int64 `json:"outage_bytes"` // during one outage
	MonthBytes  int64 `json:"month_bytes"`  // during the outages of a calendar month
}

// DataAlertsSent remembers which alerts went out, so each is sent once.
type DataAlertsSent struct {
	Outage int64  `json:"outage"` // the outage ID of the last outage alert
	Month  string `json:"month"`  // "2006-01" of the last month alert
}

// OutageTotal is the traffic of every container during one outage.
func (s *Store) OutageTotal(ctx context.Context, id int64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(rx + tx), 0) FROM outage_usage WHERE outage_id = ?`, id).Scan(&n)
	return n, err
}

// TotalSince is the traffic of every container during the outages that
// lasted past since, each counted in full.
func (s *Store) TotalSince(ctx context.Context, since time.Time) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(u.rx + u.tx), 0)
		FROM outage_usage u JOIN outages o ON o.id = u.outage_id
		WHERE o.ended IS NULL OR o.ended >= ?`, since.Unix()).Scan(&n)
	return n, err
}
