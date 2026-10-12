// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Setting keys.
const (
	SettingDetection     = "detection"      // config.Detection as JSON
	SettingSetupComplete = "setup_complete" // true once the wizard has finished
	SettingPaused        = "paused"         // true while automation is paused
)

// GetSetting decodes a JSON setting into v. It reports false if it is unset.
func (s *Store) GetSetting(ctx context.Context, key string, v any) (bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(raw), v)
}

// SetSetting stores v as JSON.
func (s *Store) SetSetting(ctx context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		key, string(b))
	return err
}

// Flag reads a boolean setting, false when unset.
func (s *Store) Flag(ctx context.Context, key string) (bool, error) {
	var v bool
	_, err := s.GetSetting(ctx, key, &v)
	return v, err
}
