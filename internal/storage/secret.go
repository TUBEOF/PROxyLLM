package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Envelope provides AES-256-GCM sealing/opening for secrets at rest.
//
// Wire format (versioned; key_id field reserved for rotation):
//
//	[0]      envelope version (1 byte, currently 0x01)
//	[1]      key_id length (1 byte; always 0 in v1 — reserved so rotation
//	         can be added without a format change)
//	[2:2+k]  key_id (skipped on open; empty in v1)
//	[..12]   fresh random 96-bit nonce — generated per encryption; nonce
//	         reuse breaks GCM (verified best practice, accessed 2026-09-10)
//	[..+16]  GCM authentication tag
//	[..]     ciphertext
//
// The stored value is a self-describing envelope: same plaintext encrypts
// to different ciphertext every time; the key never lives beside the data.
type Envelope struct {
	aead cipher.AEAD
}

// NewEnvelope constructs the AES-256-GCM envelope from a 32-byte key.
func NewEnvelope(key []byte) (*Envelope, error) {
	if len(key) != 32 {
		return nil, errors.New("storage: envelope key must be 32 bytes (AES-256)")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("storage: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("storage: new GCM: %w", err)
	}
	return &Envelope{aead: aead}, nil
}

// Seal encrypts plaintext with a fresh random nonce.
func (e *Envelope) Seal(plaintext []byte) ([]byte, error) {
	out := make([]byte, 0, 2+e.aead.NonceSize()+len(plaintext)+e.aead.Overhead())
	// Version 0x01, key_id length 0 (reserved for rotation; see type doc).
	out = append(out, 0x01, 0x00)

	nonce := make([]byte, e.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("storage: nonce: %w", err)
	}
	out = append(out, nonce...)
	return e.aead.Seal(out, nonce, plaintext, nil), nil
}

// Open decrypts and authenticates an envelope produced by Seal.
func (e *Envelope) Open(envelope []byte) ([]byte, error) {
	if len(envelope) < 2 {
		return nil, errors.New("storage: envelope truncated")
	}
	if envelope[0] != 0x01 {
		return nil, fmt.Errorf("storage: unknown envelope version 0x%02x", envelope[0])
	}
	k := int(envelope[1])
	if len(envelope) < 2+k {
		return nil, errors.New("storage: envelope key_id truncated")
	}
	// key_id is reserved for rotation (always 0 in v1); skipped without
	// interpretation. When rotation lands, the resolver maps key_id to the
	// correct key before decryption.
	body := envelope[2+k:]
	if len(body) < e.aead.NonceSize()+e.aead.Overhead() {
		return nil, errors.New("storage: envelope body truncated")
	}
	nonce, ciphertext := body[:e.aead.NonceSize()], body[e.aead.NonceSize():]
	plaintext, err := e.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("storage: open: %w", err)
	}
	return plaintext, nil
}

// Argon2Hasher hashes client keys and admin passwords per the OWASP
// Password Storage Cheat Sheet (accessed 2026-09-10): Argon2id with
// m=19 MiB, t=2, p=1 as the minimum parameter set, PHC string storage.
type Argon2Hasher struct {
	time    uint32
	memory  uint32
	threads uint8
	keyLen  uint32
	saltLen uint32
}

// NewArgon2Hasher returns a hasher with OWASP minimum parameters.
func NewArgon2Hasher() *Argon2Hasher {
	return &Argon2Hasher{
		time:    2,
		memory:  19 * 1024, // 19 MiB
		threads: 1,
		keyLen:  32,
		saltLen: 16,
	}
}

// phcB64 is the PHC-convention base64: standard alphabet, no padding.
var phcB64 = base64.RawStdEncoding

// Hash derives a PHC-format string for secret:
// $argon2id$v=19$m=<memory>,t=<time>,p=<threads>$<salt>$<hash>
func (h *Argon2Hasher) Hash(secret string) (string, error) {
	salt := make([]byte, h.saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", fmt.Errorf("storage: salt: %w", err)
	}
	dk := argon2.IDKey([]byte(secret), salt, h.time, h.memory, h.threads, h.keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, h.memory, h.time, h.threads,
		phcB64.EncodeToString(salt), phcB64.EncodeToString(dk)), nil
}

// Verify parses a PHC string and constant-time-compares a fresh derivation
// for secret. Malformed strings and below-OWASP parameter sets never
// verify.
func (h *Argon2Hasher) Verify(phc, secret string) bool {
	params, salt, want, err := parsePHC(phc)
	if err != nil {
		return false
	}
	// len(want) is bounded by the decoded PHC hash (well under 2^31).
	dk := argon2.IDKey([]byte(secret), salt, params.time, params.memory, params.threads, uint32(len(want))) //nolint:gosec // bounded input
	return len(dk) == len(want) &&
		subtle.ConstantTimeCompare(dk, want) == 1
}

type phcParams struct {
	time    uint32
	memory  uint32
	threads uint8
}

func parsePHC(phc string) (phcParams, []byte, []byte, error) {
	// "$argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>" — the leading "$"
	// makes Split produce an empty first element; the trailing "$" ends
	// the hash, so a well-formed string splits into 6 elements.
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return phcParams{}, nil, nil, errors.New("storage: malformed PHC string")
	}
	version, err := strconv.Atoi(strings.TrimPrefix(parts[2], "v="))
	if err != nil || version < 16 {
		return phcParams{}, nil, nil, errors.New("storage: bad version")
	}
	p, err := parsePHCParams(parts[3])
	if err != nil {
		return phcParams{}, nil, nil, err
	}
	if p.memory < 19*1024 || p.time < 2 || p.threads < 1 {
		// Reject below-OWASP parameter sets rather than silently verify.
		return phcParams{}, nil, nil, errors.New("storage: parameters below OWASP minimum")
	}
	// Reject absurd values (e.g. m=4 GiB) that parse fine but would OOM
	// the process during verification; 1 GiB is far above any legitimate
	// configured cost and still allows future parameter increases.
	if p.memory > 1024*1024 {
		return phcParams{}, nil, nil, errors.New("storage: parameters exceed sanity ceiling")
	}
	salt, err := phcB64.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return phcParams{}, nil, nil, errors.New("storage: bad salt")
	}
	want, err := phcB64.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return phcParams{}, nil, nil, errors.New("storage: bad hash")
	}
	return p, salt, want, nil
}

// parsePHCParams parses "m=19456,t=2,p=1".
func parsePHCParams(s string) (phcParams, error) {
	var p phcParams
	var time, threads int
	if _, err := fmt.Sscanf(s, "m=%d,t=%d,p=%d", &p.memory, &time, &threads); err != nil {
		return p, err
	}
	if time < 0 || threads < 0 || threads > 255 {
		return p, errors.New("storage: parameters out of range")
	}
	p.time = uint32(time)
	p.threads = uint8(threads)
	return p, nil
}
