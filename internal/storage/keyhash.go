package storage

// HMAC-based client-key hashes.
//
// A client key is 256 bits of CSPRNG output (see auth.GenerateClientKey), and
// Argon2id exists to make a LOW-ENTROPY secret expensive to guess — there is
// nothing here to brute-force. What it cost instead was real and per request: a
// 19 MiB working set and two iterations on EVERY authenticated request, which is
// the throughput ceiling once several hundred clients are talking to the proxy.
//
// A keyed MAC answers the same question — "is this the key whose hash we
// stored?" — in microseconds, and the master-key derivation keeps a stolen
// database alone insufficient to test guesses offline. The format carries its
// scheme, so legacy Argon2id rows keep verifying and are upgraded on first
// successful use (see auth): a mixed table is valid, and no key has to be
// reissued.
//
// Master-key coupling, stated plainly: a client-key hash is only verifiable
// while the master key that derived its MAC is available. Rotating the master
// key therefore invalidates every client key (they are display-once — the
// secrets cannot be recovered to be re-hashed). Rotating the master key already
// requires re-entering provider credentials, so this widens an existing
// operation rather than introducing a new one; the versioned prefix leaves room
// for a multi-key ring if that ever becomes worth building.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

const (
	// keyHashScheme prefixes every stored hash so verification can dispatch on
	// it. The auth package's legacy rows carry argon2Prefix instead.
	keyHashScheme = "$hmacsha256$v=1$"

	// keyHashSaltBytes is the per-hash salt. The MAC key is already secret, so
	// the salt is not what makes guessing hard — it makes two rows holding the
	// same secret hash differently, so a stolen table cannot be compared against
	// itself to find duplicate keys.
	keyHashSaltBytes = 16

	// keyHashDerivationLabel domain-separates this use of the master key from
	// the secret envelope's, so the two derivations can never collide.
	keyHashDerivationLabel = "proxyllm.client-key-hash/v1"
)

// KeyHasher hashes and verifies client keys with HMAC-SHA256 under a key derived
// from the master key.
type KeyHasher struct {
	macKey []byte
}

// NewKeyHasher derives the hasher's MAC key from the master key. The derivation
// happens once, here — never per request.
func NewKeyHasher(masterKey []byte) *KeyHasher {
	mac := hmac.New(sha256.New, masterKey)
	mac.Write([]byte(keyHashDerivationLabel))
	return &KeyHasher{macKey: mac.Sum(nil)}
}

// KeyID identifies the key material a stored hash was built with (never the
// material itself). Recorded so a future rotation can tell WHICH generation a
// row belongs to.
func (h *KeyHasher) KeyID() string {
	sum := sha256.Sum256(h.macKey)
	return hex.EncodeToString(sum[:8])
}

// Hash returns the storable hash of one secret:
// $hmacsha256$v=1$<salt-b64>$<mac-b64>.
func (h *KeyHasher) Hash(secret string) (string, error) {
	salt := make([]byte, keyHashSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return keyHashScheme + base64.RawURLEncoding.EncodeToString(salt) + "$" +
		base64.RawURLEncoding.EncodeToString(h.mac(salt, secret)), nil
}

// Verify reports whether secret produced stored. Constant-time comparison
// (hmac.Equal) and fail-closed on anything malformed.
func (h *KeyHasher) Verify(stored, secret string) bool {
	if !strings.HasPrefix(stored, keyHashScheme) {
		return false
	}
	saltB64, macB64, found := strings.Cut(strings.TrimPrefix(stored, keyHashScheme), "$")
	if !found {
		return false
	}
	salt, err := base64.RawURLEncoding.DecodeString(saltB64)
	if err != nil {
		return false
	}
	want, err := base64.RawURLEncoding.DecodeString(macB64)
	if err != nil {
		return false
	}
	return hmac.Equal(want, h.mac(salt, secret))
}

// mac computes the keyed hash of salt‖secret.
func (h *KeyHasher) mac(salt []byte, secret string) []byte {
	m := hmac.New(sha256.New, h.macKey)
	m.Write(salt)
	m.Write([]byte(secret))
	return m.Sum(nil)
}
