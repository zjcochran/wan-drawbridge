// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SettingDataAlertsSent remembers which data alerts went out (DataAlertsSent).
const SettingDataAlertsSent = "data_alerts_sent"

// DataAlertsSent remembers which data alerts went out, so each is sent once.
type DataAlertsSent struct {
	Outage int64             `json:"outage"` // the outage ID of the last outage alert
	Months map[string]string `json:"months"` // "2006-01" of the last month alert, by ISP ID
}

// DataLimits are an ISP's limits on the containers' traffic during its
// outages, in bytes; 0 is off.
type DataLimits struct {
	OutageBytes int64 // during one outage
	MonthBytes  int64 // during the ISP's outages in a calendar month
}

// SetDataLimits sets an ISP's data limits.
func (s *Store) SetDataLimits(ctx context.Context, id int, l DataLimits) error {
	res, err := s.db.ExecContext(ctx, `UPDATE isps SET limit_outage_bytes = ?, limit_month_bytes = ? WHERE id = ?`,
		max(l.OutageBytes, 0), max(l.MonthBytes, 0), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("unknown ISP %d", id)
	}
	return nil
}

// OutageLimits returns the ISP of an outage and its data limits; the ISP
// is 0 while none was seen.
func (s *Store) OutageLimits(ctx context.Context, outage int64) (int, DataLimits, error) {
	var isp int
	var l DataLimits
	err := s.db.QueryRowContext(ctx, `SELECT i.id, i.limit_outage_bytes, i.limit_month_bytes
		FROM outages o JOIN isps i ON i.id = o.isp_id WHERE o.id = ?`, outage).Scan(&isp, &l.OutageBytes, &l.MonthBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, DataLimits{}, nil
	}
	return isp, l, err
}

// OutageTotal is the traffic of every container during one outage.
func (s *Store) OutageTotal(ctx context.Context, id int64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(rx + tx), 0) FROM outage_usage WHERE outage_id = ?`, id).Scan(&n)
	return n, err
}

// ISPTotalSince is the traffic of every container during the ISP's outages
// that lasted past since, each counted in full.
func (s *Store) ISPTotalSince(ctx context.Context, isp int, since time.Time) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(u.rx + u.tx), 0)
		FROM outage_usage u JOIN outages o ON o.id = u.outage_id
		WHERE o.isp_id = ? AND (o.ended IS NULL OR o.ended >= ?)`, isp, since.Unix()).Scan(&n)
	return n, err
}

// PrimaryMeter returns the primary ISP and its monthly limit when traffic
// is on it now, it is metered and it has a monthly limit; isp is 0 otherwise.
func (s *Store) PrimaryMeter(ctx context.Context) (isp int, monthBytes int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT i.id, i.limit_month_bytes
		FROM wan_state w JOIN asns a ON a.asn = w.current_asn JOIN isps i ON i.id = a.isp_id
		WHERE i.role = 'primary' AND i.primary_metered = 1 AND i.limit_month_bytes > 0`).Scan(&isp, &monthBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	return isp, monthBytes, err
}

// RecordISPUsage adds a container's traffic since its last sample to an
// ISP's month, like RecordUsage does for an outage.
func (s *Store) RecordISPUsage(ctx context.Context, isp int, month string, hostID int64, key, containerID string, rx, tx int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO isp_usage (isp_id, month, host_id, container, container_id, last_rx, last_tx, ts) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (isp_id, month, host_id, container) DO UPDATE SET
			rx = rx + CASE WHEN container_id = excluded.container_id AND excluded.last_rx >= last_rx AND excluded.last_tx >= last_tx
				THEN excluded.last_rx - last_rx ELSE excluded.last_rx END,
			tx = tx + CASE WHEN container_id = excluded.container_id AND excluded.last_rx >= last_rx AND excluded.last_tx >= last_tx
				THEN excluded.last_tx - last_tx ELSE excluded.last_tx END,
			container_id = excluded.container_id, last_rx = excluded.last_rx, last_tx = excluded.last_tx, ts = excluded.ts`,
		isp, month, hostID, key, containerID, rx, tx, now.Unix())
	return err
}

// ISPMonthUsage is the traffic of every container on an ISP in a month
// ("YYYY-MM"), as RecordISPUsage recorded it.
func (s *Store) ISPMonthUsage(ctx context.Context, isp int, month string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(rx + tx), 0) FROM isp_usage WHERE isp_id = ? AND month = ?`, isp, month).Scan(&n)
	return n, err
}

// moveGlobalDataLimitsSQL gives the global data limits of a backup made
// before they were per ISP to every backup ISP, as migration 18 does.
const moveGlobalDataLimitsSQL = `
UPDATE isps SET
	limit_outage_bytes = COALESCE((SELECT json_extract(value, '$.outage_bytes') FROM settings WHERE key = 'data_alerts'), 0),
	limit_month_bytes = COALESCE((SELECT json_extract(value, '$.month_bytes') FROM settings WHERE key = 'data_alerts'), 0)
WHERE role = 'backup' AND EXISTS (SELECT 1 FROM settings WHERE key = 'data_alerts');
DELETE FROM settings WHERE key IN ('data_alerts', 'data_alerts_sent');`
