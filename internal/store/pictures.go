// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Where a profile picture came from.
const (
	PictureUpload = "upload" // the user uploaded it
	PictureSSO    = "sso"    // fetched from the single sign-on provider
)

// Picture is a user's profile picture.
type Picture struct {
	ContentType string
	Data        []byte
	Source      string // PictureUpload or PictureSSO
	UpdatedAt   time.Time
}

// SetUserPicture stores a user's profile picture, replacing any earlier one.
// A picture from single sign-on never replaces an uploaded one; stored
// reports whether it was stored.
func (s *Store) SetUserPicture(ctx context.Context, userID int, contentType string, data []byte, source string, now time.Time) (stored bool, err error) {
	if source != PictureUpload && source != PictureSSO {
		return false, fmt.Errorf("picture source must be upload or sso, not %q", source)
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO user_pictures (user_id, content_type, data, source, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET content_type = excluded.content_type, data = excluded.data,
			source = excluded.source, updated_at = excluded.updated_at
		WHERE excluded.source = 'upload' OR user_pictures.source = 'sso'`,
		userID, contentType, data, source, now.Unix())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// DeleteUserPicture removes a user's profile picture from source, if that is
// where the stored one came from.
func (s *Store) DeleteUserPicture(ctx context.Context, userID int, source string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_pictures WHERE user_id = ? AND source = ?`, userID, source)
	return err
}

// UserPicture returns a user's profile picture; ok is false if there is none.
func (s *Store) UserPicture(ctx context.Context, userID int) (p Picture, ok bool, err error) {
	var at int64
	err = s.db.QueryRowContext(ctx, `SELECT content_type, data, source, updated_at FROM user_pictures WHERE user_id = ?`, userID).
		Scan(&p.ContentType, &p.Data, &p.Source, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Picture{}, false, nil
	}
	if err != nil {
		return Picture{}, false, err
	}
	p.UpdatedAt = time.Unix(at, 0)
	return p, true, nil
}
