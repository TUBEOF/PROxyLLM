package anthropicoauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// newVerifier returns a PKCE code verifier: 32 random bytes, base64url without
// padding (43 chars — the RFC 7636 minimum, inside the 43–128 range).
func newVerifier() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("anthropicoauth: read verifier entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// challenge derives the S256 code challenge: base64url without padding over
// the SHA-256 digest of the verifier string.
func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// newState returns the CSRF state: 32 random bytes, base64url without padding.
func newState() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("anthropicoauth: read state entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
