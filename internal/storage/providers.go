package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// EnsureProvider seeds one provider row (id, type, name) — the FK target
// for models and provider_tokens. Idempotent: re-ensuring the same
// provider updates the display name but never silently re-types an
// existing row (a type conflict is an error for the caller to resolve —
// the WebUI provider editor, not the storage layer).
func (s *Store) EnsureProvider(ctx context.Context, id, providerType, name string) error {
	if id == "" {
		return errors.New("storage: provider without id")
	}
	if providerType == "" {
		return fmt.Errorf("storage: provider %s: missing type", id)
	}

	// Reject a type change rather than overwrite: re-typing invalidates
	// the provider's config/token rows and must be an explicit decision.
	var existing string
	err := s.db.QueryRowContext(ctx, `SELECT type FROM providers WHERE id = ?`, id).Scan(&existing)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO providers (id, type, name) VALUES (?, ?, ?)`,
			id, providerType, name)
		return err
	case err != nil:
		return err
	case existing != providerType:
		return fmt.Errorf("storage: provider %s exists with type %s (refusing re-type to %s)",
			id, existing, providerType)
	}

	_, err = s.db.ExecContext(ctx, `UPDATE providers SET name = ? WHERE id = ?`, name, id)
	return err
}
