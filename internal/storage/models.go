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
	// Origin is where the row came from (ADR-010): "manual" (operator-
	// created; the sync never touches or deletes it) or "synced"
	// (imported/updated by the model-sync run). The empty value reads as
	// "manual" — pre-v4 rows predate the sync.
	Origin string
}

// model origins (models.origin column values).
const (
	// ModelOriginManual is the operator-curated origin; the model-sync
	// never updates or deletes these rows.
	ModelManualOrigin = "manual"
	// ModelSyncedOrigin marks rows the model-sync imported; the sync
	// updates their names and deletes them when the upstream drops them.
	ModelSyncedOrigin = "synced"
)

// pinned capabilities: the capabilities blob's "pinned" object marks
// operator overrides; learning (MergeLearnedCapability) never touches them.

// PutModel upserts one model. The provider must exist — the FK is the
// referential guarantee that model rows never dangle behind a deleted
// provider. The origin persists as given: the operator's WebUI save keeps
// "manual" (the default), the sync writes "synced" — one upsert path, the
// caller owns the lifecycle semantics.
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
	if m.Origin == "" {
		m.Origin = ModelManualOrigin
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO models (id, provider_id, name, capabilities, compat, priority, origin)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			provider_id   = excluded.provider_id,
			name          = excluded.name,
			capabilities  = excluded.capabilities,
			compat        = excluded.compat,
			priority      = excluded.priority,
			origin        = excluded.origin`,
		m.ID, m.ProviderID, m.Name, string(m.Capabilities), string(m.Compat), m.Priority, m.Origin)
	return err
}

// GetModel loads one model by ID. Missing rows are ErrNotFound.
func (s *Store) GetModel(ctx context.Context, id string) (Model, error) {
	var m Model
	var caps, compat string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, provider_id, name, capabilities, compat, priority, origin
		FROM models WHERE id = ?`, id).
		Scan(&m.ID, &m.ProviderID, &m.Name, &caps, &compat, &m.Priority, &m.Origin)
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
	return s.queryModels(ctx, `SELECT id, provider_id, name, capabilities, compat, priority, origin
		FROM models ORDER BY id ASC`)
}

// ListModelsByProvider returns the models of one provider (provider detail
// drawer). Empty result is valid — a provider may have no models yet.
func (s *Store) ListModelsByProvider(ctx context.Context, providerID string) ([]Model, error) {
	return s.queryModels(ctx, `SELECT id, provider_id, name, capabilities, compat, priority, origin
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
		if err := rows.Scan(&m.ID, &m.ProviderID, &m.Name, &caps, &compat, &m.Priority, &m.Origin); err != nil {
			return nil, err
		}
		m.Capabilities = json.RawMessage(caps)
		m.Compat = json.RawMessage(compat)
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteModelsNotIn removes the provider's synced model rows that are not in
// keepIDs (ADR-010 sync policy: upstream-dropped models disappear, operator
// rows and the just-synced set survive). origin must be ModelSyncedOrigin —
// manual rows are never bulk-deleted here; any other value is refused at
// this write boundary so a sync bug cannot wipe the operator's models.
func (s *Store) DeleteModelsNotIn(ctx context.Context, providerID string, keepIDs []string, origin string) error {
	if origin != ModelSyncedOrigin {
		return fmt.Errorf("storage: bulk model delete restricted to %q origin (got %q): %w",
			ModelSyncedOrigin, origin, ErrInvalid)
	}
	if keepIDs == nil {
		keepIDs = []string{}
	}
	keep, err := json.Marshal(keepIDs)
	if err != nil {
		return fmt.Errorf("storage: encode keep set: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		DELETE FROM models
		WHERE provider_id = ? AND origin = ?
		  AND id NOT IN (SELECT value FROM json_each(?))`,
		providerID, origin, string(keep))
	return err
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

// GetModelByProviderAndName loads one model by its upstream pair (provider
// + wire name). The learning path needs this lookup: a capability_gap fires
// with the chain entry's wire model, while the row is keyed by the prefixed
// routing ID. Legacy rows (name == id) match here as well. Missing rows are
// ErrNotFound.
func (s *Store) GetModelByProviderAndName(ctx context.Context, providerID, name string) (Model, error) {
	var m Model
	var caps, compat string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, provider_id, name, capabilities, compat, priority, origin
		FROM models WHERE provider_id = ? AND name = ?`, providerID, name).
		Scan(&m.ID, &m.ProviderID, &m.Name, &caps, &compat, &m.Priority, &m.Origin)
	if errors.Is(err, sql.ErrNoRows) {
		return Model{}, fmt.Errorf("storage: model %s/%s: %w", providerID, name, ErrNotFound)
	}
	if err != nil {
		return Model{}, err
	}
	m.Capabilities = json.RawMessage(caps)
	m.Compat = json.RawMessage(compat)
	return m, nil
}

// MergeLearnedCapability writes one learned capability value into the model
// row's capabilities blob (2026-09-21 automatic capabilities). The row is
// located by the upstream pair (provider + wire name) — what a real
// upstream rejection knows. A pinned capability (operator override in the
// capabilities blob's "pinned" object) is never overwritten: manual
// selection stops the learning for exactly that flag. The row's origin and
// all other columns are untouched — learning must not flip a synced row to
// manual (the sync would otherwise stop deleting it when upstream drops
// it) and must not race the operator's model save (the read-modify-write
// runs in ONE transaction, so a concurrent PUT /api/admin/models cannot be
// silently reverted by a stale write). Unknown rows are a no-op (models
// can route without a row; learning is opportunistic), any other store
// error is returned.
func (s *Store) MergeLearnedCapability(ctx context.Context, providerID, wireName, capability string, value bool) error {
	// One transaction covers lookup + write: the operator's model editor
	// writes the same row concurrently (PUT /api/admin/models), and a
	// learning goroutine that loaded the row before that write must not
	// overwrite the operator's just-saved capabilities/pinned with stale
	// values.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var caps string
	err = tx.QueryRowContext(ctx, `
		SELECT capabilities FROM models WHERE provider_id = ? AND name = ?`,
		providerID, wireName).Scan(&caps)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // no row to learn into — nothing to do
	}
	if err != nil {
		return err
	}

	// Parse the blob defensively: unknown shapes degrade to a fresh object
	// (the write below re-serializes only the keys we know).
	blob := map[string]any{}
	if len(caps) > 0 {
		if err := json.Unmarshal([]byte(caps), &blob); err != nil {
			blob = map[string]any{}
		}
	}

	pinned, _ := blob["pinned"].(map[string]any)
	if pinned == nil {
		pinned = map[string]any{}
	}
	if pinned[capability] == true {
		return nil // operator override wins — learning stops for this flag
	}
	blob[capability] = value
	blob["pinned"] = pinned

	raw, err := json.Marshal(blob)
	if err != nil {
		return fmt.Errorf("storage: encode learned capabilities: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE models SET capabilities = ? WHERE provider_id = ? AND name = ?`,
		string(raw), providerID, wireName); err != nil {
		return err
	}
	return tx.Commit()
}
