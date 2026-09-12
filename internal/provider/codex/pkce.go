package codex

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// NewVerifier returns a fresh PKCE code verifier: 64 cryptographically random
// bytes, base64url-encoded WITHOUT padding (86 chars, inside the RFC 7636
// 43–128 range; X21 — mirroring codex-rs/login/src/pkce.rs).
func NewVerifier() (string, error) {
	buf := make([]byte, 64)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("codex: read verifier entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Challenge derives the S256 code challenge: base64url without padding over
// the SHA-256 digest of the verifier STRING bytes (X21).
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
