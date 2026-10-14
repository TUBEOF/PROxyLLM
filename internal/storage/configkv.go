package storage

import (
	"context"
	"database/sql"
	"errors"
)

// Spec: the config table is the top layer of the config
// layering (defaults → file → ENV → WebUI/DB overrides). Values are opaque
// strings at this layer; typed parsing belongs to the config package.

// ConfigEntry is one config row (sorted key order for stable rendering).
type ConfigEntry struct {
	Key   string
	Value string
}

// GetConfigValue reads one override. Absent keys return ok=false, never an
// error: "no override" is the normal state, indistinguishable from "unset"
// for the layering resolution.
func (s *Store) GetConfigValue(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM config WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// SetConfigValue upserts one override (last write wins — the WebUI save
// path).
func (s *Store) SetConfigValue(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO config (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// DeleteConfigValue removes one override. Idempotent: deleting an absent
// key reconciles toward "fall through to the lower layers" — the target
// state is achieved either way, so no ErrNotFound.
func (s *Store) DeleteConfigValue(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM config WHERE key = ?`, key)
	return err
}

// ListConfigValues returns all overrides, key-ascending, so the settings
// page renders a stable list.
func (s *Store) ListConfigValues(ctx context.Context) ([]ConfigEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM config ORDER BY key ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []ConfigEntry
	for rows.Next() {
		var e ConfigEntry
		if err := rows.Scan(&e.Key, &e.Value); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
