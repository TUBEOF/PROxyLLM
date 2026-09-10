package storage

import (
	"bytes"
	"strings"
	"testing"
)

// Spec: ADR-007 control 2 — AES-256-GCM envelope per secret, fresh random
// 12-byte nonce per encryption (verified live 2026-09-10), versioned
// envelope with key_id; Argon2id per OWASP (m=19MiB t=2 p=1 minimum,
// PHC string storage).

func TestEnvelope_RoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	e, err := NewEnvelope(key)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}

	plaintext := []byte("super-secret-oauth-refresh-token")
	sealed, err := e.Seal(plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed, plaintext) {
		t.Error("ciphertext contains plaintext")
	}

	got, err := e.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("round trip mismatch: got %q", got)
	}
}

func TestEnvelope_FreshNoncePerEncryption(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	e, err := NewEnvelope(key)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	a, _ := e.Seal([]byte("same plaintext"))
	b, _ := e.Seal([]byte("same plaintext"))
	if bytes.Equal(a, b) {
		t.Error("nonce reuse detected: identical ciphertext for identical plaintext")
	}
}

func TestEnvelope_WrongKeyFails(t *testing.T) {
	key1 := bytes.Repeat([]byte{1}, 32)
	key2 := bytes.Repeat([]byte{2}, 32)
	e1, _ := NewEnvelope(key1)
	e2, _ := NewEnvelope(key2)
	sealed, _ := e1.Seal([]byte("secret"))
	if _, err := e2.Open(sealed); err == nil {
		t.Error("decryption with wrong key must fail (GCM auth)")
	}
}

func TestEnvelope_TamperDetection(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	e, _ := NewEnvelope(key)
	sealed, _ := e.Seal([]byte("secret"))
	sealed[len(sealed)-1] ^= 0xFF
	if _, err := e.Open(sealed); err == nil {
		t.Error("tampered ciphertext must fail authentication")
	}
}

func TestEnvelope_InvalidKeySize(t *testing.T) {
	if _, err := NewEnvelope(bytes.Repeat([]byte{1}, 16)); err == nil {
		t.Error("non-256-bit key must be rejected")
	}
}

func TestEnvelope_TruncatedInput(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	e, _ := NewEnvelope(key)
	// Header-prefixed garbage: a valid version byte (0x01) followed by
	// truncated/random bytes must fail — never silently "decrypt".
	header := []byte{0x01, 0x00} // version 1, key_id length 0
	for _, n := range []int{0, 1, 5, 12, 16, 28} {
		if _, err := e.Open(append(header, bytes.Repeat([]byte{0xA5}, n)...)); err == nil {
			t.Errorf("truncated envelope (%d body bytes) must fail", n)
		}
	}
	// All-zero envelopes (version 0x00) must fail the version check.
	for _, n := range []int{2, 5, 30, 64} {
		if _, err := e.Open(bytes.Repeat([]byte{0}, n)); err == nil {
			t.Errorf("zero envelope (%d bytes) must fail", n)
		}
	}
}

func TestEnvelope_UnknownVersionRejected(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	e, _ := NewEnvelope(key)
	sealed, _ := e.Seal([]byte("secret"))
	// Flip version byte on a well-formed envelope: 0x02 must be rejected
	// without touching the body.
	sealed[0] = 0x02
	if _, err := e.Open(sealed); err == nil {
		t.Error("envelope with version 0x02 must be rejected")
	}
}

// Argon2id per OWASP Password Storage Cheat Sheet (accessed 2026-09-10):
// m=19 MiB, t=2, p=1 minimum; PHC string format.

func TestArgon2_HashAndVerify(t *testing.T) {
	h := NewArgon2Hasher()
	phc, err := h.Hash("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !h.Verify(phc, "correct-horse-battery-staple") {
		t.Error("correct password must verify")
	}
	if h.Verify(phc, "wrong-password") {
		t.Error("wrong password must not verify")
	}
}

func TestArgon2_PHCStringFormat(t *testing.T) {
	h := NewArgon2Hasher()
	phc, err := h.Hash("pw")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !strings.HasPrefix(phc, "$argon2id$") {
		t.Errorf("PHC string must start with $argon2id$: %q", phc)
	}
}

func TestArgon2_RandomSaltPerHash(t *testing.T) {
	h := NewArgon2Hasher()
	a, _ := h.Hash("same")
	b, _ := h.Hash("same")
	if a == b {
		t.Error("salt reuse detected: identical hashes for identical passwords")
	}
}

func TestArgon2_VerifyMalformed(t *testing.T) {
	h := NewArgon2Hasher()
	for _, bad := range []string{"", "not-a-phc-string", "$argon2id$v=19$m=65536,t=3,p=4$", "$argon2i$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA"} {
		if h.Verify(bad, "pw") {
			t.Errorf("malformed PHC string %q must not verify", bad)
		}
	}
}

// TestArgon2_BelowOWASPRejected pins the security gate: a well-formed PHC
// string whose parameters fall below the OWASP minimum (m=19 MiB, t=2,
// p=1) must never verify — otherwise a swapped DB row could silently
// downgrade credential checking to cheap-to-attack parameters.
func TestArgon2_BelowOWASPRejected(t *testing.T) {
	h := NewArgon2Hasher()
	// Correct algorithm, 6 parts, valid version — only the parameters are
	// below minimum. Each variant degrades exactly one knob.
	cases := []string{
		"$argon2id$v=19$m=19455,t=2,p=1$c2FsdA$aGFzaA", // m below 19 MiB
		"$argon2id$v=19$m=1024,t=2,p=1$c2FsdA$aGFzaA",  // far below
		"$argon2id$v=19$m=19456,t=1,p=1$c2FsdA$aGFzaA", // t below 2
		"$argon2id$v=19$m=19456,t=2,p=0$c2FsdA$aGFzaA", // p below 1
		"$argon2id$v=19$m=1024,t=1,p=1$c2FsdA$aGFzaA",  // all below
		"$argon2d$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA",  // weaker variant
	}
	for _, phc := range cases {
		if h.Verify(phc, "pw") {
			t.Errorf("below-OWASP PHC %q must not verify", phc)
		}
	}
}

// TestArgon2_ParameterOverflowRejected pins the parse robustness: negative
// or overflowing parameter tokens must be rejected by the parser itself
// (fmt.Sscanf rejects them — "expected integer" / "unsigned integer
// overflow") — never interpreted as huge values that could OOM Verify.
func TestArgon2_ParameterOverflowRejected(t *testing.T) {
	h := NewArgon2Hasher()
	cases := []string{
		"$argon2id$v=19$m=-1,t=2,p=1$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=99999999999,t=2,p=1$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=19456,t=-5,p=1$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=19456,t=2,p=-1$c2FsdA$aGFzaA",
		// Parseable but absurd (m=2 GiB) — rejected by the sanity ceiling.
		"$argon2id$v=19$m=2097152,t=2,p=1$c2FsdA$aGFzaA",
	}
	for _, phc := range cases {
		if h.Verify(phc, "pw") {
			t.Errorf("overflow/absurd parameter PHC %q must not verify", phc)
		}
	}
}
