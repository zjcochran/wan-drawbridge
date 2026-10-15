// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Container policies.
const (
	Inherit = "inherit"
	Disable = "disable"
	Keep    = "keep"
)

// Effective resolves a container's policy. The most specific setting wins: a
// setting made in the UI, then the container's drawbridge.on-failover label,
// then its stack's setting. Otherwise the container keeps running.
func Effective(policy, label string, stackDisabled bool) string {
	if policy != Inherit {
		return policy
	}
	if label != "" {
		return label
	}
	if stackDisabled {
		return Disable
	}
	return Keep
}

// InventoryItem is one container as seen in Docker.
type InventoryItem struct {
	Key, Project, Service, Name string
	Label                       string // from drawbridge.on-failover: "", Disable or Keep
}

// GoneAfter is how long a container can be missing from Docker before its row
// is deleted. It covers a re-create (compose up after a pull), which removes
// the old container before starting the new one, so settings survive that.
const GoneAfter = 10 * time.Minute

// SyncInventory records the containers currently on one host. That host's rows
// for containers missing for GoneAfter are deleted, and so are its stacks left
// with no containers; other hosts are untouched, so a host that cannot be
// reached keeps its rows. A new container in a stack that already has
// per-container settings is flagged for review: it keeps running until
// someone decides, unless its label says what to do.
func (s *Store) SyncInventory(ctx context.Context, hostID int64, items []InventoryItem, now time.Time) (added []string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, it := range items {
		if it.Project != "" {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO stacks (host_id, project, updated_at) VALUES (?, ?, ?) ON CONFLICT (host_id, project) DO NOTHING`,
				hostID, it.Project, now.Unix()); err != nil {
				return nil, err
			}
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM containers WHERE host_id = ? AND key = ?)`, hostID, it.Key).Scan(&exists); err != nil {
			return nil, err
		}
		if exists {
			// A label added later settles a pending review.
			if _, err := tx.ExecContext(ctx, `
				UPDATE containers SET name = ?, last_seen = ?, label_policy = ?,
					needs_review = needs_review AND ? = ''
				WHERE host_id = ? AND key = ?`, it.Name, now.Unix(), it.Label, it.Label, hostID, it.Key); err != nil {
				return nil, err
			}
			continue
		}
		var review bool
		if it.Project != "" && it.Label == "" {
			if err := tx.QueryRowContext(ctx,
				`SELECT EXISTS (SELECT 1 FROM containers WHERE host_id = ? AND project = ? AND policy <> 'inherit')`,
				hostID, it.Project).Scan(&review); err != nil {
				return nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO containers (host_id, key, project, service, name, label_policy, needs_review, first_seen, last_seen)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			hostID, it.Key, it.Project, it.Service, it.Name, it.Label, review, now.Unix(), now.Unix()); err != nil {
			return nil, err
		}
		added = append(added, it.Key)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM containers WHERE host_id = ? AND last_seen < ?`, hostID, now.Add(-GoneAfter).Unix()); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM stacks WHERE host_id = ? AND project NOT IN (SELECT project FROM containers WHERE host_id = ?)`, hostID, hostID); err != nil {
		return nil, err
	}
	return added, tx.Commit()
}

// SetStackPolicy turns a stack's "disable on failover" on or off. Either way,
// every container in the stack goes back to following the stack.
func (s *Store) SetStackPolicy(ctx context.Context, hostID int64, project string, disable bool, actor string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE stacks SET disable_on_failover = ?, updated_at = ?, updated_by = ? WHERE host_id = ? AND project = ?`,
		disable, now.Unix(), actor, hostID, project)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("unknown stack %q", project)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE containers SET policy = 'inherit', needs_review = 0 WHERE host_id = ? AND project = ?`, hostID, project); err != nil {
		return err
	}
	return tx.Commit()
}

// SetContainerPolicy sets one container's policy. Overriding a container that
// inherits "disable" from its stack converts the stack rule into per-container
// rules: the stack setting is cleared and every other container in the stack
// keeps being disabled through its own setting (containers with a label keep
// following it). It reports whether that happened.
func (s *Store) SetContainerPolicy(ctx context.Context, hostID int64, key, policy, actor string, now time.Time) (converted bool, err error) {
	if policy != Inherit && policy != Disable && policy != Keep {
		return false, fmt.Errorf("policy must be inherit, disable or keep, not %q", policy)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var project string
	var stackDisabled sql.NullBool
	err = tx.QueryRowContext(ctx, `
		SELECT c.project, st.disable_on_failover FROM containers c
		LEFT JOIN stacks st ON st.host_id = c.host_id AND st.project = c.project
		WHERE c.host_id = ? AND c.key = ?`, hostID, key).Scan(&project, &stackDisabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("unknown container %q", key)
	}
	if err != nil {
		return false, err
	}

	if stackDisabled.Bool && policy == Keep {
		converted = true
		if _, err := tx.ExecContext(ctx,
			`UPDATE stacks SET disable_on_failover = 0, updated_at = ?, updated_by = ? WHERE host_id = ? AND project = ?`,
			now.Unix(), actor, hostID, project); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE containers SET policy = 'disable' WHERE host_id = ? AND project = ? AND key <> ? AND policy = 'inherit' AND label_policy = ''`,
			hostID, project, key); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE containers SET policy = ?, needs_review = 0 WHERE host_id = ? AND key = ?`, policy, hostID, key); err != nil {
		return false, err
	}
	return converted, tx.Commit()
}

// ContainerRow is a container's stored settings with its effective policy.
type ContainerRow struct {
	HostID                      int64
	Host                        string // the host's name
	Key, Project, Service, Name string
	Policy                      string
	Label                       string // from the drawbridge.on-failover label
	StackDisabled               bool
	Effective                   string
	NeedsReview                 bool
	LastSeen                    time.Time
}

// Containers returns every known container, ordered by host, stack, then key.
func (s *Store) Containers(ctx context.Context) ([]ContainerRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.host_id, COALESCE(h.name, ''), c.key, c.project, c.service, c.name, c.policy, c.label_policy,
			COALESCE(st.disable_on_failover, 0), c.needs_review, c.last_seen
		FROM containers c
		LEFT JOIN stacks st ON st.host_id = c.host_id AND st.project = c.project
		LEFT JOIN hosts h ON h.id = c.host_id
		ORDER BY c.host_id, c.project, c.key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContainerRow
	for rows.Next() {
		var r ContainerRow
		var seen int64
		if err := rows.Scan(&r.HostID, &r.Host, &r.Key, &r.Project, &r.Service, &r.Name, &r.Policy, &r.Label, &r.StackDisabled, &r.NeedsReview, &seen); err != nil {
			return nil, err
		}
		r.Effective = Effective(r.Policy, r.Label, r.StackDisabled)
		r.LastSeen = time.Unix(seen, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// StackRow summarises a stack for the Stacks screen.
type StackRow struct {
	HostID     int64
	Host       string // the host's name
	Project    string
	Disabled   bool // the stack-wide setting
	Containers int
	Disabling  int // containers whose effective policy is disable
}

// Status is "disabled", "partial" or "none".
func (r StackRow) Status() string {
	switch {
	case r.Disabled:
		return "disabled"
	case r.Disabling > 0:
		return "partial"
	}
	return "none"
}

// Stacks returns every Compose project with its setting and container
// counts, ordered by host then project.
func (s *Store) Stacks(ctx context.Context) ([]StackRow, error) {
	cs, err := s.Containers(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT st.host_id, COALESCE(h.name, ''), st.project, st.disable_on_failover
		FROM stacks st LEFT JOIN hosts h ON h.id = st.host_id
		ORDER BY st.host_id, st.project`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StackRow
	for rows.Next() {
		var r StackRow
		if err := rows.Scan(&r.HostID, &r.Host, &r.Project, &r.Disabled); err != nil {
			return nil, err
		}
		for _, c := range cs {
			if c.HostID == r.HostID && c.Project == r.Project {
				r.Containers++
				if c.Effective == Disable {
					r.Disabling++
				}
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
