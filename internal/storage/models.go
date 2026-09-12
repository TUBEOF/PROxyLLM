package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Model is one persisted model row (plan Analysis C schema + §6.4): the
// per-model compatibility policy lives in Compat JSON, WebUI-editable via
// the "Models & Compatibility" area.
type Model struct {
	ID           string
	ProviderID   string
	Name         string
	Capabilities json.RawMessage
	Compat       json.RawMessage
	Priority     int
}

// PutModel upserts one model. The provider must exist — the FK is the
// referential guarantee that model rows never dangle behind a deleted
// provider.
func (s *Store) PutModel(ctx context.Context, m Model) error {
	if m.ID == "" {
		return fmt.Errorf("model without id: %w", ErrInvalid)
	}
	if m.ProviderID == "" {
		return fmt.Errorf("model %s: missing provider_id: %w", m.ID, ErrInvalid)
	}
	if len(m.Capabilities) == 0 {
		m.Capabilities = json.RawMessage("{}")
	}
	if len(m.Compat) == 0 {
		m.Compat = json.RawMessage("{}")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO models (id, provider_id, name, capabilities, compat, priority)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			provider_id   = excluded.provider_id,
			name          = excluded.name,
			capabilities  = excluded.capabilities,
			compat        = excluded.compat,
			priority      = excluded.priority`,
		m.ID, m.ProviderID, m.Name, string(m.Capabilities), string(m.Compat), m.Priority)
	return err
}

// GetModel loads one model by ID. Missing rows are ErrNotFound.
func (s *Store) GetModel(ctx context.Context, id string) (Model, error) {
	var m Model
	var caps, compat string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, provider_id, name, capabilities, compat, priority
		FROM models WHERE id = ?`, id).
		Scan(&m.ID, &m.ProviderID, &m.Name, &caps, &compat, &m.Priority)
	if errors.Is(err, sql.ErrNoRows) {
		return Model{}, fmt.Errorf("storage: model %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Model{}, err
	}
	m.Capabilities = json.RawMessage(caps)
	m.Compat = json.RawMessage(compat)
	return m, nil
}

// ListModels returns every model (WebUI "Models & Compatibility" list).
func (s *Store) ListModels(ctx context.Context) ([]Model, error) {
	return s.queryModels(ctx, `SELECT id, provider_id, name, capabilities, compat, priority
		FROM models ORDER BY id ASC`)
}

// ListModelsByProvider returns the models of one provider (provider detail
// drawer). Empty result is valid — a provider may have no models yet.
func (s *Store) ListModelsByProvider(ctx context.Context, providerID string) ([]Model, error) {
	return s.queryModels(ctx, `SELECT id, provider_id, name, capabilities, compat, priority
		FROM models WHERE provider_id = ? ORDER BY id ASC`, providerID)
}

// queryModels is the shared row scanner for the model list surfaces.
func (s *Store) queryModels(ctx context.Context, query string, args ...any) ([]Model, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Model
	for rows.Next() {
		var m Model
		var caps, compat string
		if err := rows.Scan(&m.ID, &m.ProviderID, &m.Name, &caps, &compat, &m.Priority); err != nil {
			return nil, err
		}
		m.Capabilities = json.RawMessage(caps)
		m.Compat = json.RawMessage(compat)
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteModel removes one model. Missing rows are ErrNotFound.
func (s *Store) DeleteModel(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM models WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("storage: model %s: %w", id, ErrNotFound)
	}
	return nil
}
