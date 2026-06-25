package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Setting represents one row in the `settings` table. Settings are runtime-
// mutable configuration values that are persisted across restarts and can be
// edited from the admin UI without redeploying the service.
//
// Keys are dotted paths ("upstream.api_key", "admin.cookie_secret") so the
// table can grow to hold arbitrary runtime config without schema changes.
type Setting struct {
	Key       string `json:"key"`
	Value     string `json:"-"`
	UpdatedAt int64  `json:"updated_at"`
}

// ErrSettingNotFound is returned by GetSetting when the key is absent.
var ErrSettingNotFound = errors.New("setting not found")

// GetSetting fetches a single setting by key. Returns ErrSettingNotFound
// when the row does not exist so callers can distinguish "missing" from
// "empty value".
func (s *Store) GetSetting(ctx context.Context, key string) (Setting, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT key, value, updated_at FROM settings WHERE key = ?`, key)
	var v Setting
	if err := row.Scan(&v.Key, &v.Value, &v.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Setting{}, ErrSettingNotFound
		}
		return Setting{}, err
	}
	return v, nil
}

// SetSetting upserts a setting. The updated_at timestamp is refreshed to
// "now" so the UI can show when the value last changed.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	now := time.Now().UnixMilli()
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`, key, value, now)
	return err
}

// DeleteSetting removes a key. Returns nil if the row was already absent —
// delete is idempotent so callers don't need to pre-check.
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
	return err
}

// ListSettings returns every persisted setting, sorted by key. Useful for
// the admin "Settings" page so the operator can see what's been set.
func (s *Store) ListSettings(ctx context.Context) ([]Setting, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT key, value, updated_at FROM settings ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Setting, 0, 4)
	for rows.Next() {
		var v Setting
		if err := rows.Scan(&v.Key, &v.Value, &v.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
