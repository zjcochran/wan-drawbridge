// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Agent enrollment tokens.
const (
	EnrollPrefix   = "dbe_"
	EnrollTokenTTL = time.Hour
)

// Enrollment errors.
var (
	ErrEnrollToken   = errors.New("unknown or already used enrollment token")
	ErrEnrollExpired = errors.New("the enrollment token has expired; make a new one in Settings → Hosts")
)

// hostName is a valid host name: it goes in front of a colon in CLI
// arguments, so it has none.
var hostName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

// ValidHostName checks a host name.
func ValidHostName(name string) error {
	if !hostName.MatchString(name) {
		return errors.New("a host name is 1 to 32 letters, digits, dots, dashes or underscores, starting with a letter or digit")
	}
	return nil
}

// AgentHost is a host reached through an agent, with what the manager keeps
// about it.
type AgentHost struct {
	HostRow
	Version       string
	PublicKey     string // the agent's current key; "" until it enrolls, or when revoked
	PreviousKey   string // its key before the last rotation, until the new one is used
	KeyCreated    time.Time
	EnrollPending bool // a one-time token waits to be used
	EnrollExpires time.Time
	LastAddress   string
	Inventory     string // JSON of the workloads it last pushed
	ManagerKeyID  string // the manager key it has pinned
}

const agentColumns = `id, name, kind, platform, status, last_seen, last_error, version, public_key, previous_key,
	key_created, enroll_hash <> '', enroll_expires, last_address, inventory, manager_key_id FROM hosts`

func scanAgent(sc interface{ Scan(...any) error }) (AgentHost, error) {
	var a AgentHost
	var seen, created, expires sql.NullInt64
	err := sc.Scan(&a.ID, &a.Name, &a.Kind, &a.Platform, &a.Status, &seen, &a.LastError, &a.Version, &a.PublicKey,
		&a.PreviousKey, &created, &a.EnrollPending, &expires, &a.LastAddress, &a.Inventory, &a.ManagerKeyID)
	if seen.Valid {
		a.LastSeen = time.Unix(seen.Int64, 0)
	}
	if created.Valid {
		a.KeyCreated = time.Unix(created.Int64, 0)
	}
	if expires.Valid {
		a.EnrollExpires = time.Unix(expires.Int64, 0)
	}
	return a, err
}

// AgentHosts returns every agent host, by ID.
func (s *Store) AgentHosts(ctx context.Context) ([]AgentHost, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+agentColumns+` WHERE kind = 'agent' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentHost
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Agent returns one agent host.
func (s *Store) Agent(ctx context.Context, id int64) (AgentHost, bool, error) {
	a, err := scanAgent(s.db.QueryRowContext(ctx, `SELECT `+agentColumns+` WHERE kind = 'agent' AND id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return AgentHost{}, false, nil
	}
	return a, err == nil, err
}

// AddAgentHost records a new agent host, waiting to enroll, and returns its
// ID and one-time enrollment token. This is the only time the token can be
// read.
func (s *Store) AddAgentHost(ctx context.Context, name, actor string, now time.Time) (int64, string, error) {
	name = strings.TrimSpace(name)
	if err := ValidHostName(name); err != nil {
		return 0, "", err
	}
	token := EnrollPrefix + randomToken()
	// Parameterized: every value is a bound argument, not SQL text.
	// nosemgrep
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO hosts (name, kind, status, enroll_hash, enroll_expires, created_at, created_by)
		VALUES (?, 'agent', 'pending', ?, ?, ?, ?)`,
		name, hashToken(token), now.Add(EnrollTokenTTL).Unix(), now.Unix(), actor)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, "", fmt.Errorf("a host named %q already exists", name)
		}
		return 0, "", err
	}
	id, err := res.LastInsertId()
	return id, token, err
}

// NewEnrollToken makes a new one-time token for an agent host. Its current
// key stops working: the agent must enroll again with the token.
func (s *Store) NewEnrollToken(ctx context.Context, id int64, now time.Time) (string, error) {
	token := EnrollPrefix + randomToken()
	return token, s.updateAgent(ctx, id, `
		UPDATE hosts SET enroll_hash = ?, enroll_expires = ?, public_key = '', previous_key = '', status = 'pending'
		WHERE id = ? AND kind = 'agent'`, hashToken(token), now.Add(EnrollTokenTTL).Unix(), id)
}

// Enrollment is what an agent sends to enroll.
type Enrollment struct {
	Token, PublicKey, Platform, Version, Address, ManagerKeyID string
}

// Enroll uses a one-time token: the agent host it belongs to takes the
// agent's key and comes online. The token cannot be used again.
func (s *Store) Enroll(ctx context.Context, e Enrollment, now time.Time) (AgentHost, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentHost{}, err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	var id int64
	var expires sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT id, enroll_expires FROM hosts WHERE kind = 'agent' AND enroll_hash = ?`,
		hashToken(e.Token)).Scan(&id, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentHost{}, ErrEnrollToken
	}
	if err != nil {
		return AgentHost{}, err
	}
	if !expires.Valid || now.Unix() > expires.Int64 {
		return AgentHost{}, ErrEnrollExpired
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE hosts SET public_key = ?, previous_key = '', key_created = ?, enroll_hash = '', enroll_expires = NULL,
			status = 'online', platform = ?, version = ?, last_address = ?, last_seen = ?, last_error = '', manager_key_id = ?
		WHERE id = ?`,
		e.PublicKey, now.Unix(), e.Platform, e.Version, e.Address, now.Unix(), e.ManagerKeyID, id); err != nil {
		return AgentHost{}, err
	}
	a, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentColumns+` WHERE id = ?`, id))
	if err != nil {
		return AgentHost{}, err
	}
	return a, tx.Commit()
}

func (s *Store) updateAgent(ctx context.Context, id int64, q string, args ...any) error {
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no agent host %d", id)
	}
	return nil
}

// RotateAgentKey replaces an agent's key. The old key keeps working until
// the new one is first used (RetirePreviousKey), so a rotation cut short
// cannot lock the agent out.
func (s *Store) RotateAgentKey(ctx context.Context, id int64, newKey string, now time.Time) error {
	return s.updateAgent(ctx, id, `
		UPDATE hosts SET previous_key = public_key, public_key = ?, key_created = ?
		WHERE id = ? AND kind = 'agent' AND public_key <> ''`, newKey, now.Unix(), id)
}

// RetirePreviousKey drops an agent's previous key once the new one is used.
func (s *Store) RetirePreviousKey(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE hosts SET previous_key = '' WHERE id = ?`, id)
	return err
}

// AgentSeen records an authenticated request from an agent.
func (s *Store) AgentSeen(ctx context.Context, id int64, address string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE hosts SET last_seen = ?, last_address = ?, last_error = '',
			status = CASE WHEN status IN ('revoked', 'disabled') THEN status ELSE 'online' END
		WHERE id = ?`, now.Unix(), address, id)
	return err
}

// SaveInventory keeps the workloads an agent pushed, with its platform and
// version.
func (s *Store) SaveInventory(ctx context.Context, id int64, inventory, platform, version string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE hosts SET inventory = ?, platform = ?, version = ? WHERE id = ?`,
		inventory, platform, version, id)
	return err
}

// SetManagerKeyID records which manager key an agent has pinned.
func (s *Store) SetManagerKeyID(ctx context.Context, id int64, keyID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE hosts SET manager_key_id = ? WHERE id = ?`, keyID, id)
	return err
}

// RevokeHost cuts an agent off at once: its keys and any enrollment token
// are cleared.
func (s *Store) RevokeHost(ctx context.Context, id int64) error {
	return s.updateAgent(ctx, id, `
		UPDATE hosts SET public_key = '', previous_key = '', enroll_hash = '', enroll_expires = NULL, status = 'revoked'
		WHERE id = ? AND kind = 'agent'`, id)
}

// SetHostEnabled disables a host, which takes it out of every failover, or
// enables it again.
func (s *Store) SetHostEnabled(ctx context.Context, id int64, enabled bool) error {
	q := `UPDATE hosts SET status = 'disabled' WHERE id = ? AND status <> 'revoked'`
	if enabled {
		q = `UPDATE hosts SET status = CASE WHEN kind = 'agent' AND public_key = '' THEN 'pending' ELSE 'offline' END
			WHERE id = ? AND status = 'disabled'`
	}
	_, err := s.db.ExecContext(ctx, q, id)
	return err
}

// RenameHost renames a host.
func (s *Store) RenameHost(ctx context.Context, id int64, name string) error {
	name = strings.TrimSpace(name)
	if err := ValidHostName(name); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE hosts SET name = ? WHERE id = ?`, name, id)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("a host named %q already exists", name)
	}
	return err
}

// ForgetPending drops the starts still owed to a host.
func (s *Store) ForgetPending(ctx context.Context, hostID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE run_actions SET pending = 0, error = 'forgotten' WHERE host_id = ? AND pending = 1`, hostID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RemoveHost deletes an agent host with its containers, stacks and owed
// starts. Its history (events, data used) is kept.
func (s *Store) RemoveHost(ctx context.Context, id int64) error {
	if id == LocalHost {
		return errors.New("the local host cannot be removed")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	for _, q := range []string{
		`UPDATE run_actions SET pending = 0, error = 'host removed' WHERE host_id = ? AND pending = 1`,
		`DELETE FROM containers WHERE host_id = ?`,
		`DELETE FROM stacks WHERE host_id = ?`,
		`DELETE FROM hosts WHERE id = ? AND kind = 'agent'`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
