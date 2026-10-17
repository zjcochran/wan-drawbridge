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

// moveGlobalDataLimitsSQL gives the global data limits of a backup made
// before they were per ISP to every backup ISP, as migration 18 does.
const moveGlobalDataLimitsSQL = `
UPDATE isps SET
	limit_outage_bytes = COALESCE((SELECT json_extract(value, '$.outage_bytes') FROM settings WHERE key = 'data_alerts'), 0),
	limit_month_bytes = COALESCE((SELECT json_extract(value, '$.month_bytes') FROM settings WHERE key = 'data_alerts'), 0)
WHERE role = 'backup' AND EXISTS (SELECT 1 FROM settings WHERE key = 'data_alerts');
DELETE FROM settings WHERE key IN ('data_alerts', 'data_alerts_sent');`
