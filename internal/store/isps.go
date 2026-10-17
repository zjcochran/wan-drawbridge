// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/learner"
)

// ISP roles.
const (
	RolePrimary = "primary"
	RoleBackup  = "backup"
	RoleIgnored = "ignored"
)

// GroupKey derives the ISP group for an ASN from its name: the AS handle up to
// the first part that is an AS number. So EXAMPLENET-64500 and EXAMPLENET-64501 are
// both EXAMPLENET, METROCABLE-64503-WA-OR-C3 is METROCABLE and SKYLINK-AS64510 is
// SKYLINK, while BIGTEL-INTERNET4 and BIGTEL-MOBILITY-LLC-AS64511 (BigTel fiber and
// BigTel cellular) stay apart. Names that yield nothing group by the AS number.
func GroupKey(name string, asn int) string {
	fields := strings.Fields(name)
	if len(fields) == 0 {
		return fmt.Sprintf("AS%d", asn)
	}
	handle := strings.ToUpper(strings.TrimRight(fields[0], ",.;"))
	var kept []string
	for _, part := range strings.Split(handle, "-") {
		if part == "" || digits(part) || part == "AS" || (strings.HasPrefix(part, "AS") && digits(part[2:])) {
			break
		}
		kept = append(kept, part)
	}
	if len(kept) == 0 {
		return fmt.Sprintf("AS%d", asn)
	}
	return strings.Join(kept, "-")
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Metered reports whether an ISP group is metered; an unknown one is.
func (s *Store) Metered(ctx context.Context, id int) (bool, error) {
	metered := true
	err := s.db.QueryRowContext(ctx, `SELECT metered FROM isps WHERE id = ?`, id).Scan(&metered)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	return metered, err
}

// SetMetered marks an ISP group metered or unmetered.
func (s *Store) SetMetered(ctx context.Context, id int, metered bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE isps SET metered = ? WHERE id = ?`, metered, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("unknown ISP %d", id)
	}
	return nil
}

// SeeASN records that asn was seen and returns its ISP group and role. A new
// ASN joins the group its name maps to, creating the group if needed.
func (s *Store) SeeASN(ctx context.Context, asn int, name string, now time.Time) (ispID int, role string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback()

	newISP := false
	err = tx.QueryRowContext(ctx, `SELECT isp_id FROM asns WHERE asn = ?`, asn).Scan(&ispID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		key := GroupKey(name, asn)
		err = tx.QueryRowContext(ctx, `SELECT id FROM isps WHERE group_key = ?`, key).Scan(&ispID)
		if errors.Is(err, sql.ErrNoRows) {
			res, ierr := tx.ExecContext(ctx,
				`INSERT INTO isps (group_key, name, first_seen, last_seen) VALUES (?, ?, ?, ?)`,
				key, key, now.Unix(), now.Unix())
			if ierr != nil {
				return 0, "", ierr
			}
			id, _ := res.LastInsertId()
			ispID, err, newISP = int(id), nil, true
		}
		if err != nil {
			return 0, "", err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO asns (asn, name, isp_id, first_seen, last_seen) VALUES (?, ?, ?, ?, ?)`,
			asn, name, ispID, now.Unix(), now.Unix()); err != nil {
			return 0, "", err
		}
	case err != nil:
		return 0, "", err
	default:
		if _, err := tx.ExecContext(ctx, `
			UPDATE asns SET last_seen = ?, name = CASE WHEN ? <> '' THEN ? ELSE name END WHERE asn = ?`,
			now.Unix(), name, name, asn); err != nil {
			return 0, "", err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE isps SET last_seen = ? WHERE id = ?`, now.Unix(), ispID); err != nil {
		return 0, "", err
	}
	if err := tx.QueryRowContext(ctx, `SELECT role FROM isps WHERE id = ?`, ispID).Scan(&role); err != nil {
		return 0, "", err
	}
	if err := tx.Commit(); err != nil {
		return 0, "", err
	}
	if newISP {
		err = s.AddEvent(ctx, now, "isp_new", map[string]any{"isp": ispID, "name": GroupKey(name, asn), "asn": asn})
	}
	return ispID, role, err
}

// ISPOfASN returns the ISP group an ASN belongs to, or 0 if it was never seen.
func (s *Store) ISPOfASN(ctx context.Context, asn int) (int, error) {
	var id int
	err := s.db.QueryRowContext(ctx, `SELECT isp_id FROM asns WHERE asn = ?`, asn).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// Observe adds seconds of observed time to an ASN's totals.
func (s *Store) Observe(ctx context.Context, asn int, seconds int64, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE asns SET observed_seconds = observed_seconds + ? WHERE asn = ?`, seconds, asn); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO asn_daily (asn, day, observed_seconds) VALUES (?, ?, ?)
		ON CONFLICT (asn, day) DO UPDATE SET observed_seconds = observed_seconds + excluded.observed_seconds`,
		asn, day(now), seconds); err != nil {
		return err
	}
	return tx.Commit()
}

// Shares returns observed time per non-ignored ISP group for days on or after
// since. Time is kept per ASN and summed through the current grouping, so a
// merge or split takes effect on history too.
func (s *Store) Shares(ctx context.Context, since time.Time) ([]learner.Share, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.isp_id, SUM(d.observed_seconds) FROM asn_daily d
		JOIN asns a ON a.asn = d.asn
		JOIN isps i ON i.id = a.isp_id AND i.role <> 'ignored'
		WHERE d.day >= ? GROUP BY a.isp_id`, day(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []learner.Share
	for rows.Next() {
		var sh learner.Share
		if err := rows.Scan(&sh.ID, &sh.Seconds); err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// Primary returns the primary ISP group (0 if none) and whether it is pinned.
func (s *Store) Primary(ctx context.Context) (id int, pinned bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT id, pinned FROM isps WHERE role = 'primary'`).Scan(&id, &pinned)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, pinned, err
}

// SetRole sets an ISP group's role. Making one primary demotes the previous
// primary to backup; a primary set this way is pinned unless pinned is false.
func (s *Store) SetRole(ctx context.Context, id int, role string, pinned bool) error {
	if role != RolePrimary && role != RoleBackup && role != RoleIgnored {
		return fmt.Errorf("role must be primary, backup or ignored, not %q", role)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if role == RolePrimary {
		if _, err := tx.ExecContext(ctx,
			`UPDATE isps SET role = 'backup', pinned = 0 WHERE role = 'primary' AND id <> ?`, id); err != nil {
			return err
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE isps SET role = ?, pinned = ? WHERE id = ?`,
		role, role == RolePrimary && pinned, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("unknown ISP %d", id)
	}
	return tx.Commit()
}

// MoveASN moves an ASN into another ISP group (to = 0 makes a new group for it
// alone). Moved ASNs are marked manual. Groups left with no ASNs are deleted
// unless primary. It returns the group the ASN ended up in.
func (s *Store) MoveASN(ctx context.Context, asn, to int, now time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var name string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM asns WHERE asn = ?`, asn).Scan(&name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("ASN %d has not been seen", asn)
		}
		return 0, err
	}
	if to == 0 {
		label := fmt.Sprintf("AS%d", asn)
		if name != "" {
			label = name
		}
		// Parameterized: every value is a bound argument, not SQL text.
		// nosemgrep
		res, err := tx.ExecContext(ctx,
			`INSERT INTO isps (group_key, name, first_seen, last_seen) VALUES (?, ?, ?, ?)`,
			fmt.Sprintf("manual-AS%d", asn), label, now.Unix(), now.Unix())
		if err != nil {
			return 0, fmt.Errorf("ASN %d already has its own group: %w", asn, err)
		}
		id, _ := res.LastInsertId()
		to = int(id)
	} else {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM isps WHERE id = ?)`, to).Scan(&exists); err != nil {
			return 0, err
		}
		if !exists {
			return 0, fmt.Errorf("unknown ISP %d", to)
		}
	}
	// Parameterized: every value is a bound argument, not SQL text.
	// nosemgrep
	if _, err := tx.ExecContext(ctx, `UPDATE asns SET isp_id = ?, manual = 1 WHERE asn = ?`, to, asn); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM isps WHERE role <> 'primary' AND NOT EXISTS (SELECT 1 FROM asns WHERE asns.isp_id = isps.id)`); err != nil {
		return 0, err
	}
	return to, tx.Commit()
}

// ISPRow is an ISP group with its ASNs, for listing.
type ISPRow struct {
	ID          int
	Name        string // display name if set, else the group key
	GroupKey    string
	Role        string
	Pinned      bool
	Metered     bool // only a metered backup triggers a failover
	Limits      DataLimits
	WeekSeconds int64
	AllSeconds  int64
	LastSeen    time.Time
	ASNs        []ASNRow
}

// ASNRow is one ASN in an ISP group; Manual means it was grouped by hand.
type ASNRow struct {
	ASN    int
	Name   string
	Manual bool
}

// ISPs lists every ISP group, primary first, then by time seen this week.
func (s *Store) ISPs(ctx context.Context, now time.Time) ([]ISPRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, COALESCE(NULLIF(i.display_name, ''), i.name), i.group_key, i.role, i.pinned, i.metered, i.limit_outage_bytes, i.limit_month_bytes, i.last_seen,
			COALESCE((SELECT SUM(d.observed_seconds) FROM asn_daily d JOIN asns a ON a.asn = d.asn
				WHERE a.isp_id = i.id AND d.day >= ?), 0) AS week,
			COALESCE((SELECT SUM(a.observed_seconds) FROM asns a WHERE a.isp_id = i.id), 0)
		FROM isps i ORDER BY i.role = 'primary' DESC, week DESC, i.id`, day(now.Add(-7*24*time.Hour)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ISPRow
	for rows.Next() {
		var r ISPRow
		var seen int64
		if err := rows.Scan(&r.ID, &r.Name, &r.GroupKey, &r.Role, &r.Pinned, &r.Metered, &r.Limits.OutageBytes, &r.Limits.MonthBytes, &seen, &r.WeekSeconds, &r.AllSeconds); err != nil {
			return nil, err
		}
		r.LastSeen = time.Unix(seen, 0)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close() // done before the per-ISP queries below
	for i := range out {
		if out[i].ASNs, err = s.asnsOf(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// asnsOf lists the ASNs in one ISP group.
func (s *Store) asnsOf(ctx context.Context, ispID int) ([]ASNRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT asn, name, manual FROM asns WHERE isp_id = ? ORDER BY asn`, ispID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ASNRow
	for rows.Next() {
		var a ASNRow
		if err := rows.Scan(&a.ASN, &a.Name, &a.Manual); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RenameISP sets an ISP group's display name ("" resets it to the group name).
func (s *Store) RenameISP(ctx context.Context, id int, name string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE isps SET display_name = ? WHERE id = ?`, strings.TrimSpace(name), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("unknown ISP %d", id)
	}
	return nil
}

// ISPNames maps ISP group IDs to their display names.
func (s *Store) ISPNames(ctx context.Context) (map[int]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, COALESCE(NULLIF(display_name, ''), name) FROM isps`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var id int
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// ClearPrimary demotes the primary ISP to backup, so the learner decides again.
func (s *Store) ClearPrimary(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE isps SET role = 'backup', pinned = 0 WHERE role = 'primary'`)
	return err
}
