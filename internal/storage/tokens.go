package storage

import (
	"context"
	"database/sql"
	"errors"
)

// ErrNotFound is returned when a typed row does not exist (token, model,
// rule). Callers match with errors.Is.
var ErrNotFound = errors.New("storage: not found")

// ErrInvalid is returned when a write is rejected by validation (missing
// id/type, empty chain, secret-shaped config key …) — as opposed to a missing
// row (ErrNotFound) or an internal failure. The admin API maps it to 400
// without matching on message strings.
var ErrInvalid = errors.New("storage: invalid")

// ProviderToken is one decrypted provider credential row. Secrets exist in
// plaintext ONLY inside this struct — at rest they are AES-256-GCM
// envelopes (ADR-sec-storage; research digest X12).
type ProviderToken struct {
	ProviderID  string
	Kind        string // e.g. "access_token", "refresh_token", "id_token"
	Secret      []byte
	ExpiresAt   *int64 // unix seconds, nil when open-ended
	LastRefresh *int64
}

// PutProviderTokens atomically upserts the given tokens: all rows seal with
// a fresh nonce and commit in ONE transaction — a refresh rotation that
// persists access + refresh tokens either lands whole or not at all (X12).
// Requires the envelope (SetEnvelope); refusing the write is safer than
// persisting plaintext.
func (s *Store) PutProviderTokens(ctx context.Context, tokens ...ProviderToken) error {
	if len(tokens) == 0 {
		return nil
	}
	e := s.Envelope()
	if e == nil {
		return errors.New("storage: secret envelope not initialized — refusing to persist plaintext token")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, tok := range tokens {
		sealed, err := e.Seal(tok.Secret)
		if err != nil {
			return err
		}
		var expires, lastRefresh any
		if tok.ExpiresAt != nil {
			expires = *tok.ExpiresAt
		}
		if tok.LastRefresh != nil {
			lastRefresh = *tok.LastRefresh
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO provider_tokens (provider_id, kind, token_encrypted, expires_at, last_refresh)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(provider_id, kind) DO UPDATE SET
				token_encrypted = excluded.token_encrypted,
				expires_at      = excluded.expires_at,
				last_refresh    = excluded.last_refresh`,
			tok.ProviderID, tok.Kind, sealed, expires, lastRefresh); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetProviderToken reads and opens one token envelope. Missing rows are
// ErrNotFound.
func (s *Store) GetProviderToken(ctx context.Context, providerID, kind string) (*ProviderToken, error) {
	e := s.Envelope()
	if e == nil {
		return nil, errors.New("storage: secret envelope not initialized")
	}

	var sealed []byte
	var expires, lastRefresh sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT token_encrypted, expires_at, last_refresh
		FROM provider_tokens WHERE provider_id = ? AND kind = ?`,
		providerID, kind).Scan(&sealed, &expires, &lastRefresh)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	plaintext, err := e.Open(sealed)
	if err != nil {
		return nil, err
	}
	tok := &ProviderToken{
		ProviderID: providerID,
		Kind:       kind,
		Secret:     plaintext,
	}
	if expires.Valid {
		v := expires.Int64
		tok.ExpiresAt = &v
	}
	if lastRefresh.Valid {
		v := lastRefresh.Int64
		tok.LastRefresh = &v
	}
	return tok, nil
}

// DeleteProviderToken removes one token row. Missing rows are ErrNotFound.
func (s *Store) DeleteProviderToken(ctx context.Context, providerID, kind string) error {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM provider_tokens WHERE provider_id = ? AND kind = ?`,
		providerID, kind)
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
	return nil
}
