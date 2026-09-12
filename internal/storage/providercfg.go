package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Provider is the CRUD-facing provider row (plan Analysis C: id, type, name
// + per-type non-secret config). The Config blob carries public settings
// only (base_url etc.) — credentials NEVER ride here; they live in
// provider_tokens behind the AES-256-GCM envelope.
type Provider struct {
	ID     string
	Type   string
	Name   string
	Config json.RawMessage
}

// secretShapedConfigKeys are config keys whose values must never appear in
// the providers.config column — defense-in-depth for Analysis G control 2
// ("secrets never in config exports"): even if the API layer's validation
// were bypassed, the storage boundary refuses to persist a plaintext
// credential here.
var secretShapedConfigKeys = []string{"api_key", "apikey", "token", "secret", "password", "refresh_token", "access_token"}

// rejectSecretConfig walks the decoded config object and fails on any
// secret-shaped key, at any nesting depth.
func rejectSecretConfig(cfg json.RawMessage) error {
	if len(cfg) == 0 {
		return nil
	}
	var obj any
	if err := json.Unmarshal(cfg, &obj); err != nil {
		return fmt.Errorf("provider config is not valid JSON: %w: %w", ErrInvalid, err)
	}
	var walk func(v any) error
	walk = func(v any) error {
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				for _, bad := range secretShapedConfigKeys {
					if k == bad {
						return fmt.Errorf("provider config must not contain key %q — secrets belong in the encrypted token store: %w", k, ErrInvalid)
					}
				}
				if err := walk(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range t {
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(obj)
}

// PutProvider upserts one provider row — the WebUI editor's save path.
// Unlike EnsureProvider (seed path, refuses re-typing), an explicit save may
// change type or name: last write wins.
func (s *Store) PutProvider(ctx context.Context, p Provider) error {
	if p.ID == "" {
		return fmt.Errorf("provider without id: %w", ErrInvalid)
	}
	if p.Type == "" {
		return fmt.Errorf("provider %s: missing type: %w", p.ID, ErrInvalid)
	}
	if err := rejectSecretConfig(p.Config); err != nil {
		return err
	}
	if len(p.Config) == 0 {
		p.Config = json.RawMessage("{}")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO providers (id, type, name, config) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			type   = excluded.type,
			name   = excluded.name,
			config = excluded.config`,
		p.ID, p.Type, p.Name, string(p.Config))
	return err
}

// GetProvider reads one provider row. Missing rows are ErrNotFound.
func (s *Store) GetProvider(ctx context.Context, id string) (*Provider, error) {
	var p Provider
	var cfg string
	err := s.db.QueryRowContext(ctx, `SELECT id, type, name, config FROM providers WHERE id = ?`, id).
		Scan(&p.ID, &p.Type, &p.Name, &cfg)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.Config = json.RawMessage(cfg)
	return &p, nil
}

// ListProviders returns all providers, id-ascending, so the WebUI table has
// a stable order between requests.
func (s *Store) ListProviders(ctx context.Context) ([]Provider, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, type, name, config FROM providers ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Provider
	for rows.Next() {
		var p Provider
		var cfg string
		if err := rows.Scan(&p.ID, &p.Type, &p.Name, &cfg); err != nil {
			return nil, err
		}
		p.Config = json.RawMessage(cfg)
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeleteProvider removes a provider together with its models and its
// (encrypted) token rows in one transaction — no orphan rows, no credential
// residue for a provider the operator removed. Missing rows are
// ErrNotFound.
func (s *Store) DeleteProvider(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// A provider may legitimately exist without a token row; the provider
	// delete below decides the ErrNotFound outcome.
	if _, err := tx.ExecContext(ctx, `DELETE FROM provider_tokens WHERE provider_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM models WHERE provider_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM providers WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}
