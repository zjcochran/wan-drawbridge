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

// APITokenPrefix starts every API token, so a leaked one is recognisable.
const APITokenPrefix = "dbt_"

// APIToken is a token for the JSON API, without its secret: only a hash is
// stored.
type APIToken struct {
	ID        int64
	Name      string
	CreatedAt time.Time
	CreatedBy string
	LastUsed  time.Time // zero if never used
}

// CreateAPIToken makes a token named name and returns it. This is the only
// time the token can be read.
func (s *Store) CreateAPIToken(ctx context.Context, name, by string, now time.Time) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return "", errors.New("the name must be 1 to 64 characters")
	}
	token := APITokenPrefix + randomToken()
	// Parameterized: every value is a bound argument, not SQL text.
	// nosemgrep
	_, err := s.db.ExecContext(ctx, `INSERT INTO api_tokens (name, hash, created_at, created_by) VALUES (?, ?, ?, ?)`,
		name, hashToken(token), now.Unix(), by)
	return token, err
}

// APITokens lists the tokens, oldest first.
func (s *Store) APITokens(ctx context.Context) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created_at, created_by, last_used FROM api_tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		var t APIToken
		var created, used int64
		if err := rows.Scan(&t.ID, &t.Name, &created, &t.CreatedBy, &used); err != nil {
			return nil, err
		}
		t.CreatedAt, t.LastUsed = time.Unix(created, 0), fromUnix(used)
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteAPIToken revokes a token and returns its name.
func (s *Store) DeleteAPIToken(ctx context.Context, id int64) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx, `DELETE FROM api_tokens WHERE id = ? RETURNING name`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("unknown token %d", id)
	}
	return name, err
}

// UseAPIToken checks a presented token and, when it is valid, records the
// use and returns the token's name.
func (s *Store) UseAPIToken(ctx context.Context, token string, now time.Time) (name string, ok bool, err error) {
	if !strings.HasPrefix(token, APITokenPrefix) {
		return "", false, nil
	}
	err = s.db.QueryRowContext(ctx, `UPDATE api_tokens SET last_used = ? WHERE hash = ? RETURNING name`,
		now.Unix(), hashToken(token)).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return name, err == nil, err
}
