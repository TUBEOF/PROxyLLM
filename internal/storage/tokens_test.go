package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// Spec: ADR-sec-storage + research digest X12 (opposite number: atomic
// rotation persistence) — provider tokens live ONLY as AES-256-GCM
// envelopes in provider_tokens; reading returns plaintext; writes seal with
// a fresh nonce; multi-token writes are one transaction (all-or-nothing).
// Fact X12, source: codex-rs/login/src/auth/manager.rs, accessed 2026-09-10.

func openTokenStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(testCfg(filepath.Join(dir, "test.db")))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	key := make([]byte, 32)
	e, err := NewEnvelope(key)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	s.SetEnvelope(e)

	if _, err := s.db.Exec(`INSERT INTO providers (id, type, name) VALUES ('codex-1', 'codex', 'Codex')`); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	return s
}

func TestProviderToken_RoundTrip(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	expires := int64(1800000000)
	tok := ProviderToken{
		ProviderID: "codex-1",
		Kind:       "access_token",
		Secret:     []byte("ey.jwt.access"),
		ExpiresAt:  &expires,
	}
	if err := s.PutProviderTokens(ctx, tok); err != nil {
		t.Fatalf("PutProviderTokens: %v", err)
	}

	got, err := s.GetProviderToken(ctx, "codex-1", "access_token")
	if err != nil {
		t.Fatalf("GetProviderToken: %v", err)
	}
	if string(got.Secret) != "ey.jwt.access" {
		t.Errorf("secret round-trip: got %q want %q", got.Secret, "ey.jwt.access")
	}
	if got.ExpiresAt == nil || *got.ExpiresAt != expires {
		t.Errorf("expires_at round-trip: got %v want %d", got.ExpiresAt, expires)
	}
}

func TestProviderToken_MissingIsErrNotFound(t *testing.T) {
	s := openTokenStore(t)

	if _, err := s.GetProviderToken(context.Background(), "codex-1", "access_token"); err != nil {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("missing token error = %v, want ErrNotFound", err)
		}
	} else {
		t.Error("missing token must return ErrNotFound, got nil")
	}
}

func TestProviderToken_SealedAtRest(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	secret := "ey.jwt.refresh" //nolint:gosec // dummy fixture value, never a real credential
	if err := s.PutProviderTokens(ctx, ProviderToken{
		ProviderID: "codex-1",
		Kind:       "refresh_token",
		Secret:     []byte(secret),
	}); err != nil {
		t.Fatalf("PutProviderTokens: %v", err)
	}

	// The raw column must NOT contain the plaintext.
	var raw []byte
	if err := s.db.QueryRow(`SELECT token_encrypted FROM provider_tokens WHERE provider_id='codex-1' AND kind='refresh_token'`).Scan(&raw); err != nil {
		t.Fatalf("query raw column: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("token_encrypted is empty")
	}
	if stringContains(raw, secret) {
		t.Error("token_encrypted contains plaintext — secrets must be sealed (ADR-sec-storage)")
	}

	// Re-encrypts differently per write (fresh nonce, X12/aead).
	if err := s.PutProviderTokens(ctx, ProviderToken{
		ProviderID: "codex-1",
		Kind:       "refresh_token",
		Secret:     []byte(secret),
	}); err != nil {
		t.Fatalf("PutProviderTokens second: %v", err)
	}
	var raw2 []byte
	if err := s.db.QueryRow(`SELECT token_encrypted FROM provider_tokens WHERE provider_id='codex-1' AND kind='refresh_token'`).Scan(&raw2); err != nil {
		t.Fatal(err)
	}
	if string(raw) == string(raw2) {
		t.Error("sealing must use a fresh nonce — identical ciphertext for identical plaintext")
	}
}

func TestProviderToken_MultiWriteAtomic(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	// X12: access + rotated refresh persist in ONE transaction.
	if err := s.PutProviderTokens(ctx,
		ProviderToken{ProviderID: "codex-1", Kind: "access_token", Secret: []byte("a1")},
		ProviderToken{ProviderID: "codex-1", Kind: "refresh_token", Secret: []byte("r1")},
	); err != nil {
		t.Fatalf("seed tokens: %v", err)
	}

	// The token write itself must never leave a half-updated set: a row
	// targeting an unknown provider fails the WHOLE batch (FK), and the
	// earlier rows in the same call must not have been written.
	err := s.PutProviderTokens(ctx,
		ProviderToken{ProviderID: "codex-1", Kind: "access_token", Secret: []byte("a2")},
		ProviderToken{ProviderID: "nope", Kind: "access_token", Secret: []byte("x")},
	)
	if err == nil {
		t.Fatal("batch with unknown provider must fail (FK violation)")
	}

	got, err := s.GetProviderToken(ctx, "codex-1", "access_token")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Secret) != "a1" {
		t.Errorf("failed batch leaked a partial write: access_token = %q, want a1 (all-or-nothing, X12)", got.Secret)
	}
}

func TestProviderToken_Delete(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	if err := s.PutProviderTokens(ctx, ProviderToken{
		ProviderID: "codex-1", Kind: "access_token", Secret: []byte("a"),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.DeleteProviderToken(ctx, "codex-1", "access_token"); err != nil {
		t.Fatalf("DeleteProviderToken: %v", err)
	}
	if err := s.DeleteProviderToken(ctx, "codex-1", "access_token"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting a missing token = %v, want ErrNotFound", err)
	}
	if _, err := s.GetProviderToken(ctx, "codex-1", "access_token"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted token readable: %v", err)
	}
}

func TestProviderToken_NoEnvelopeFails(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(testCfg(filepath.Join(dir, "test.db")))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.db.Exec(`INSERT INTO providers (id, type, name) VALUES ('codex-1', 'codex', 'Codex')`); err != nil {
		t.Fatalf("seed provider: %v", err)
	}

	// Without a master key, secret writes must refuse rather than persist
	// plaintext (ADR-sec-storage: tokens only in the encrypted store).
	err = s.PutProviderTokens(context.Background(), ProviderToken{
		ProviderID: "codex-1", Kind: "access_token", Secret: []byte("a"),
	})
	if err == nil {
		t.Error("write without envelope must fail")
	}
}

// stringContains reports whether the plaintext bytes appear verbatim in the
// sealed blob (a negative control — envelopes must never contain them).
func stringContains(haystack []byte, needle string) bool {
	n := len(needle)
	for i := 0; i+n <= len(haystack); i++ {
		if string(haystack[i:i+n]) == needle {
			return true
		}
	}
	return false
}
