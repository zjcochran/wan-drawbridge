// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
)

// RecoveryCodeCount is how many recovery codes a user gets at a time.
const RecoveryCodeCount = 10

// NewRecoveryCodes returns fresh codes to show the user once, like
// "k3f9-x2pq-7hwd", and their hashes to store.
func NewRecoveryCodes() (codes, hashes []string) {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789" // no 0/o, 1/l/i
	for range RecoveryCodeCount {
		b := make([]byte, 12)
		if _, err := rand.Read(b); err != nil {
			panic(err)
		}
		code := make([]byte, 0, 14)
		for i := range 12 {
			if i > 0 && i%4 == 0 {
				code = append(code, '-')
			}
			code = append(code, alphabet[int(b[i])%len(alphabet)])
		}
		codes = append(codes, string(code))
		hashes = append(hashes, hashToken(string(code)))
	}
	return codes, hashes
}

// normalizeRecoveryCode accepts a code typed with or without dashes or
// spaces, in either case.
func normalizeRecoveryCode(code string) string {
	code = strings.ToLower(strings.NewReplacer("-", "", " ", "").Replace(code))
	if len(code) != 12 {
		return code
	}
	return code[:4] + "-" + code[4:8] + "-" + code[8:]
}

// EnableTwoFactor turns on two-factor sign-in with a sealed TOTP secret and
// replaces the user's recovery codes.
func (s *Store) EnableTwoFactor(ctx context.Context, id int, sealedSecret string, step int64, codeHashes []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	res, err := tx.ExecContext(ctx, `UPDATE users SET totp_secret = ?, totp_step = ? WHERE id = ?`, sealedSecret, step, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if err := replaceCodes(ctx, tx, id, codeHashes); err != nil {
		return err
	}
	return tx.Commit()
}

func replaceCodes(ctx context.Context, tx *sql.Tx, id int, hashes []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id = ?`, id); err != nil {
		return err
	}
	for _, h := range hashes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO recovery_codes (user_id, hash) VALUES (?, ?)`, id, h); err != nil {
			return err
		}
	}
	return nil
}

// ReplaceRecoveryCodes stores a new set of recovery codes for a user with
// two-factor sign-in on.
func (s *Store) ReplaceRecoveryCodes(ctx context.Context, id int, codeHashes []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	if err := replaceCodes(ctx, tx, id, codeHashes); err != nil {
		return err
	}
	return tx.Commit()
}

// DisableTwoFactor turns two-factor sign-in off and deletes the recovery
// codes. It reports whether it was on.
func (s *Store) DisableTwoFactor(ctx context.Context, id int) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET totp_secret = '', totp_step = 0 WHERE id = ? AND totp_secret <> ''`, id)
	if err != nil {
		return false, err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id = ?`, id); err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// TwoFactorSecret returns a user's sealed TOTP secret ("" when off).
func (s *Store) TwoFactorSecret(ctx context.Context, id int) (string, error) {
	var secret string
	err := s.db.QueryRowContext(ctx, `SELECT totp_secret FROM users WHERE id = ?`, id).Scan(&secret)
	return secret, err
}

// UseTOTPStep records that a code for step was used. It fails (false) when
// that step or a later one was used already, so a code works only once.
func (s *Store) UseTOTPStep(ctx context.Context, id int, step int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET totp_step = ? WHERE id = ? AND totp_step < ?`, step, id, step)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// UseRecoveryCode deletes a matching recovery code and reports whether there
// was one.
func (s *Store) UseRecoveryCode(ctx context.Context, id int, code string) (bool, error) {
	var hash string
	err := s.db.QueryRowContext(ctx, `DELETE FROM recovery_codes WHERE user_id = ? AND hash = ? RETURNING hash`,
		id, hashToken(normalizeRecoveryCode(code))).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// RecoveryCodesLeft counts a user's unused recovery codes.
func (s *Store) RecoveryCodesLeft(ctx context.Context, id int) (int, error) {
	var n int
	return n, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM recovery_codes WHERE user_id = ?`, id).Scan(&n)
}

// VerifyPassword checks a user's current password, for confirming changes.
func (s *Store) VerifyPassword(ctx context.Context, id int, password string) (bool, error) {
	var hash sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, id).Scan(&hash); err != nil {
		return false, err
	}
	return hash.Valid && CheckPassword(hash.String, password), nil
}
