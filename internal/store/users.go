// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// User roles.
const (
	RoleAdmin  = "admin"
	RoleViewer = "viewer"
)

// MinPasswordLength is the spec's minimum for local passwords.
const MinPasswordLength = 12

// ErrBadLogin is Authenticate's answer for an unknown user, a wrong password
// or a disabled account alike, so a sign-in reveals none of them.
var ErrBadLogin = errors.New("wrong username or password")

// User is an account; the password hash stays in the database.
type User struct {
	ID        int
	Username  string
	Role      string
	Disabled  bool
	HasLocal  bool // has a local password
	HasSSO    bool // has a linked single sign-on account
	CreatedAt time.Time
	LastLogin time.Time
	Theme     string // colour theme name; see web.Themes
	Mode      string // "auto", "light" or "dark"
	Picture   int64  // when the profile picture was stored (Unix time); 0 = none
	// SSOPicture is whether single sign-on may set the picture.
	SSOPicture bool
	TwoFactor  bool // a code is needed after the password
}

// IsAdmin reports whether the user has the admin role.
func (u User) IsAdmin() bool { return u.Role == RoleAdmin }

// argon2id parameters (RFC 9106's second recommended option).
const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
)

// HashPassword returns an encoded argon2id hash.
func HashPassword(pw string) (string, error) {
	if len(pw) < MinPasswordLength {
		return "", fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding.EncodeToString
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64(salt), b64(key)), nil
}

// CheckPassword reports whether pw matches an encoded argon2id hash.
func CheckPassword(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(want) == 0 || len(want) > 1024 {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want))) //nolint:gosec // G115: len(want) is checked to be at most 1024 above
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash keeps a failed login for an unknown user as slow as a real one.
var dummyHash, _ = HashPassword("not-a-real-password-at-all")

// CountUsers returns the number of users; 0 means setup has not run.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	return n, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
}

// CreateUser adds a user whose password is hashed with argon2id.
func (s *Store) CreateUser(ctx context.Context, username, password, role string, now time.Time) (User, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return User{}, errors.New("username is required")
	}
	if role != RoleAdmin && role != RoleViewer {
		return User{}, fmt.Errorf("role must be admin or viewer, not %q", role)
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, role, created_at) VALUES (?, ?, ?, ?)`,
		username, hash, role, now.Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return User{}, fmt.Errorf("user %q already exists", username)
		}
		return User{}, err
	}
	id, _ := res.LastInsertId()
	return s.UserByID(ctx, int(id))
}

// Authenticate checks a local login. The sign-in time is recorded here,
// unless the user has two-factor sign-in: then it is recorded with
// TouchLogin once the code is checked.
func (s *Store) Authenticate(ctx context.Context, username, password string, now time.Time) (User, error) {
	var id int
	var hash sql.NullString
	var disabled, twoFactor bool
	err := s.db.QueryRowContext(ctx,
		`SELECT id, password_hash, disabled, totp_secret <> '' FROM users WHERE username = ?`, strings.TrimSpace(username)).
		Scan(&id, &hash, &disabled, &twoFactor)
	if errors.Is(err, sql.ErrNoRows) {
		CheckPassword(dummyHash, password)
		return User{}, ErrBadLogin
	}
	if err != nil {
		return User{}, err
	}
	if !hash.Valid || !CheckPassword(hash.String, password) || disabled {
		return User{}, ErrBadLogin
	}
	if !twoFactor {
		if err := s.TouchLogin(ctx, id, now); err != nil {
			return User{}, err
		}
	}
	return s.UserByID(ctx, id)
}

const userCols = `id, username, role, disabled, password_hash IS NOT NULL,
	EXISTS (SELECT 1 FROM oidc_identities o WHERE o.user_id = users.id), created_at, COALESCE(last_login, 0),
	theme, mode, COALESCE((SELECT updated_at FROM user_pictures p WHERE p.user_id = users.id), 0), sso_picture,
	totp_secret <> ''`

func scanUser(sc interface{ Scan(...any) error }) (User, error) {
	var u User
	var created, last int64
	if err := sc.Scan(&u.ID, &u.Username, &u.Role, &u.Disabled, &u.HasLocal, &u.HasSSO, &created, &last, &u.Theme, &u.Mode, &u.Picture, &u.SSOPicture, &u.TwoFactor); err != nil {
		return User{}, err
	}
	u.CreatedAt = time.Unix(created, 0)
	if last > 0 {
		u.LastLogin = time.Unix(last, 0)
	}
	return u, nil
}

// SetAppearance saves a user's colour theme and mode. The caller validates them.
func (s *Store) SetAppearance(ctx context.Context, id int, theme, mode string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET theme = ?, mode = ? WHERE id = ?`, theme, mode, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UserByID returns the user with this ID.
func (s *Store) UserByID(ctx context.Context, id int) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

// Users returns every user, by username.
func (s *Store) Users(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetPassword replaces a user's password and ends their sessions.
func (s *Store) SetPassword(ctx context.Context, id int, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, hash, id); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id)
	return err
}

// SetUserDisabled enables or disables a user; disabling ends their sessions.
// The last enabled admin cannot be disabled.
func (s *Store) SetUserDisabled(ctx context.Context, id int, disabled bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if disabled {
		var others int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled = 0 AND id <> ?`, id).Scan(&others); err != nil {
			return err
		}
		var role string
		if err := tx.QueryRowContext(ctx, `SELECT role FROM users WHERE id = ?`, id).Scan(&role); err != nil {
			return err
		}
		if role == RoleAdmin && others == 0 {
			return errors.New("cannot disable the last admin")
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET disabled = ? WHERE id = ?`, disabled, id); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteUser deletes a user with their sessions and single sign-on links.
// The last enabled admin cannot be deleted.
func (s *Store) DeleteUser(ctx context.Context, id int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var role string
	var disabled bool
	if err := tx.QueryRowContext(ctx, `SELECT role, disabled FROM users WHERE id = ?`, id).Scan(&role, &disabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("no such user")
		}
		return err
	}
	var others int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled = 0 AND id <> ?`, id).Scan(&others); err != nil {
		return err
	}
	if role == RoleAdmin && !disabled && others == 0 {
		return errors.New("cannot delete the last admin")
	}
	// Sessions and identities go with it (ON DELETE CASCADE).
	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// Session is a logged-in browser.
type Session struct {
	User User
	CSRF string
}

// SessionTTL is how long a session lasts without activity.
const SessionTTL = 7 * 24 * time.Hour

// CreateSession returns a new random cookie token; only its hash is stored.
func (s *Store) CreateSession(ctx context.Context, userID int, ip, ua string, now time.Time) (token string, err error) {
	token, csrf := randomToken(), randomToken()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO sessions (id, user_id, csrf, created_at, expires_at, ip, user_agent) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		hashToken(token), userID, csrf, now.Unix(), now.Add(SessionTTL).Unix(), ip, truncate(ua, 256))
	return token, err
}

// SessionByToken returns the session for a cookie token and slides its expiry.
func (s *Store) SessionByToken(ctx context.Context, token string, now time.Time) (Session, bool, error) {
	var sess Session
	var uid int
	var expires int64
	id := hashToken(token)
	err := s.db.QueryRowContext(ctx, `SELECT user_id, csrf, expires_at FROM sessions WHERE id = ?`, id).
		Scan(&uid, &sess.CSRF, &expires)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && expires < now.Unix()) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, err
	}
	if sess.User, err = s.UserByID(ctx, uid); err != nil {
		return Session{}, false, err
	}
	if sess.User.Disabled {
		return Session{}, false, nil
	}
	// Slide the expiry at most once an hour, not on every request.
	if expires-int64(SessionTTL.Seconds())+3600 < now.Unix() {
		_, err = s.db.ExecContext(ctx, `UPDATE sessions SET expires_at = ? WHERE id = ?`, now.Add(SessionTTL).Unix(), id)
	}
	return sess, true, err
}

// DeleteSession signs out the session with this token.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, hashToken(token))
	return err
}

// PruneSessions deletes expired sessions.
func (s *Store) PruneSessions(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, now.Unix())
	return err
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
