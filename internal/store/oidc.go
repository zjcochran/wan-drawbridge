// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Identity is an OIDC account linked to a user.
type Identity struct {
	Issuer, Subject, Email string
	LinkedAt               time.Time
}

// UserByIdentity returns the user linked to issuer+subject, if any.
func (s *Store) UserByIdentity(ctx context.Context, issuer, subject string) (User, bool, error) {
	var id int
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id FROM oidc_identities WHERE issuer = ? AND subject = ?`, issuer, subject).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, err
	}
	u, err := s.UserByID(ctx, id)
	return u, err == nil, err
}

// LinkIdentity links an OIDC account to a user. An account already linked to
// someone else is refused.
func (s *Store) LinkIdentity(ctx context.Context, userID int, issuer, subject, email string, now time.Time) error {
	existing, ok, err := s.UserByIdentity(ctx, issuer, subject)
	if err != nil {
		return err
	}
	if ok && existing.ID != userID {
		return fmt.Errorf("this single sign-on account is already linked to %s", existing.Username)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO oidc_identities (issuer, subject, user_id, email, linked_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (issuer, subject) DO UPDATE SET email = excluded.email`,
		issuer, subject, userID, email, now.Unix())
	return err
}

// UnlinkIdentities removes a user's OIDC links. A user without a local
// password would be locked out, so that is refused.
func (s *Store) UnlinkIdentities(ctx context.Context, userID int) error {
	u, err := s.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if !u.HasLocal {
		return errors.New("set a password first: this account has no other way to sign in")
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM oidc_identities WHERE user_id = ?`, userID)
	return err
}

func (s *Store) Identities(ctx context.Context, userID int) ([]Identity, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT issuer, subject, email, linked_at FROM oidc_identities WHERE user_id = ? ORDER BY linked_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		var i Identity
		var at int64
		if err := rows.Scan(&i.Issuer, &i.Subject, &i.Email, &at); err != nil {
			return nil, err
		}
		i.LinkedAt = time.Unix(at, 0)
		out = append(out, i)
	}
	return out, rows.Err()
}

// CreateSSOUser creates a user without a local password, linked to an OIDC
// account, picking a free username based on want.
func (s *Store) CreateSSOUser(ctx context.Context, want, role, issuer, subject, email string, now time.Time) (User, error) {
	if role != RoleAdmin && role != RoleViewer {
		return User{}, fmt.Errorf("role must be admin or viewer, not %q", role)
	}
	base := strings.TrimSpace(want)
	if base == "" {
		base = "user"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	var id int64
	for n := 1; ; n++ {
		name := base
		if n > 1 {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		// Parameterized: every value is a bound argument, not SQL text.
		// nosemgrep
		res, err := tx.ExecContext(ctx,
			`INSERT INTO users (username, role, created_at) VALUES (?, ?, ?) ON CONFLICT (username) DO NOTHING`,
			name, role, now.Unix())
		if err != nil {
			return User{}, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			id, _ = res.LastInsertId()
			break
		}
		if n > 100 {
			return User{}, errors.New("no free username")
		}
	}
	// Parameterized: every value is a bound argument, not SQL text.
	// nosemgrep
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO oidc_identities (issuer, subject, user_id, email, linked_at) VALUES (?, ?, ?, ?, ?)`,
		issuer, subject, id, email, now.Unix()); err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return s.UserByID(ctx, int(id))
}

// SetUserRole changes a user's role. The last enabled admin cannot be demoted.
func (s *Store) SetUserRole(ctx context.Context, id int, role string) error {
	if role != RoleAdmin && role != RoleViewer {
		return fmt.Errorf("role must be admin or viewer, not %q", role)
	}
	if role == RoleViewer {
		var others int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled = 0 AND id <> ?`, id).Scan(&others); err != nil {
			return err
		}
		if others == 0 {
			return errors.New("cannot demote the last admin")
		}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, role, id)
	return err
}

// TouchLogin records a successful sign-in.
func (s *Store) TouchLogin(ctx context.Context, id int, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET last_login = ? WHERE id = ?`, now.Unix(), id)
	return err
}
