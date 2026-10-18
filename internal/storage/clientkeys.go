package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ClientKey is one persisted client_keys row: the key
// itself never touches the database — only the Argon2id PHC hash, the short
// display prefix, and metadata for the WebUI /keys area (G control 1).
type ClientKey struct {
	ID         string
	Name       string
	KeyHash    string
	KeyPrefix  string
	Scopes     json.RawMessage
	CreatedAt  int64
	LastUsedAt *int64 // unix seconds, nil when never used
	Revoked    bool
}

// CreateClientKey inserts a new key row. id and phcHash come from the auth
// layer (fresh key id + Argon2id hash of the display-once secret);
// scopes may be nil (stored as the empty pattern list).
func (s *Store) CreateClientKey(ctx context.Context, id, name, phcHash, keyPrefix string, scopes json.RawMessage) error {
	if len(scopes) == 0 {
		scopes = json.RawMessage("[]")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO client_keys (id, name, key_hash, key_prefix, scopes, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, name, phcHash, keyPrefix, string(scopes), time.Now().Unix())
	return err
}

// GetClientKey loads one row by id. Missing rows are ErrNotFound.
func (s *Store) GetClientKey(ctx context.Context, id string) (ClientKey, error) {
	var k ClientKey
	var scopes string
	var lastUsed sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, key_hash, key_prefix, scopes, created_at, last_used_at, revoked
		FROM client_keys WHERE id = ?`, id).
		Scan(&k.ID, &k.Name, &k.KeyHash, &k.KeyPrefix, &scopes, &k.CreatedAt, &lastUsed, &k.Revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return ClientKey{}, fmt.Errorf("storage: client key %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return ClientKey{}, err
	}
	k.Scopes = json.RawMessage(scopes)
	if lastUsed.Valid {
		k.LastUsedAt = &lastUsed.Int64
	}
	return k, nil
}

// ListClientKeys returns the CURRENT keys for the WebUI /keys area —
// deleted keys are hard-deleted (DeleteClientKey), and the migration that
// introduced that behavior purged the old soft-deleted rows. Newest first:
// the key an operator just created is the one they need to see.
func (s *Store) ListClientKeys(ctx context.Context) ([]ClientKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, key_hash, key_prefix, scopes, created_at, last_used_at, revoked
		FROM client_keys WHERE revoked = 0 ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []ClientKey
	for rows.Next() {
		var k ClientKey
		var scopes string
		var lastUsed sql.NullInt64
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyHash, &k.KeyPrefix, &scopes, &k.CreatedAt, &lastUsed, &k.Revoked); err != nil {
			return nil, err
		}
		k.Scopes = json.RawMessage(scopes)
		if lastUsed.Valid {
			k.LastUsedAt = &lastUsed.Int64
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ListClientKeysByPrefix returns all active (non-revoked) keys whose display
// prefix matches — the lookup path during verification: the prefix narrows
// candidates before the (expensive) Argon2id comparison.
func (s *Store) ListClientKeysByPrefix(ctx context.Context, prefix string) ([]ClientKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, key_hash, key_prefix, scopes, created_at, last_used_at, revoked
		FROM client_keys WHERE key_prefix = ? AND revoked = 0`, prefix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []ClientKey
	for rows.Next() {
		var k ClientKey
		var scopes string
		// The single-row scan mirrors GetClientKey.
		var lastUsed sql.NullInt64
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyHash, &k.KeyPrefix, &scopes, &k.CreatedAt, &lastUsed, &k.Revoked); err != nil {
			return nil, err
		}
		k.Scopes = json.RawMessage(scopes)
		if lastUsed.Valid {
			k.LastUsedAt = &lastUsed.Int64
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// TouchClientKey stamps last_used_at for one key. The WHERE guards revoked
// keys from being touched post-revocation.
func (s *Store) TouchClientKey(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE client_keys SET last_used_at = ? WHERE id = ? AND revoked = 0`,
		time.Now().Unix(), id)
	return err
}

// UpdateClientKeyHash rewrites one row's stored hash — the transparent upgrade
// of a legacy Argon2id hash to the keyed-MAC scheme, done on the first
// successful verify of that key. The secret is never stored, so a
// row that is never used again simply stays on the scheme it was written with;
// both schemes verify (see VerifyClientKeyHash).
func (s *Store) UpdateClientKeyHash(ctx context.Context, id, keyHash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE client_keys SET key_hash = ? WHERE id = ?`, keyHash, id)
	return err
}

// DeleteClientKey removes the row outright. The operator's rule is that a
// deleted key leaves the database, not an audit trail: the row (name,
// prefix, hash) is gone, and the key stops authenticating immediately
// because the verification path reads the table per request. Historical
// request_logs rows keep the id string (there is no foreign key); the log's
// name projection falls back to that id. Missing rows are ErrNotFound.
func (s *Store) DeleteClientKey(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM client_keys WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("storage: client key %s: %w", id, ErrNotFound)
	}
	return nil
}

// RegenerateClientKey replaces one row's secret material: a fresh hash and
// display prefix for the SAME id and name, with a new issuance time and no
// last-used stamp. The old secret stops verifying as soon as the update
// lands. Missing rows are ErrNotFound.
func (s *Store) RegenerateClientKey(ctx context.Context, id, keyHash, keyPrefix string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE client_keys
		SET key_hash = ?, key_prefix = ?, created_at = ?, last_used_at = NULL
		WHERE id = ?`,
		keyHash, keyPrefix, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("storage: client key %s: %w", id, ErrNotFound)
	}
	return nil
}
