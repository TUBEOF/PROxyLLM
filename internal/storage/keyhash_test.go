package storage

// Client-key hashing: a keyed MAC derived from the master key for
// 256-bit client secrets, Argon2id still for admin passwords, and a legacy
// Argon2id row that keeps verifying.
//
// Spec: the per-request cost of the Argon2id verify (19 MiB, t=2) is the
// throughput ceiling once several hundred clients talk to the proxy, and a
// 256-bit CSPRNG secret has nothing for a KDF to protect. What must NOT change
// is that guessing offline stays impossible from a stolen table alone, and that
// no already-issued key stops working.

import (
	"strings"
	"testing"
)

func TestKeyHasher_RoundTripAndRejects(t *testing.T) {
	const secret = "pk_abcdefgh_0mF3sQ7rT1uV9xY2zA4bC6dE8fG0hI2jK4lM6n" //nolint:gosec // a synthetic key for a hashing round trip, never a real credential
	h := NewKeyHasher([]byte("master-key-material-for-tests"))

	stored, err := h.Hash(secret)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !strings.HasPrefix(stored, "$hmacsha256$v=1$") {
		t.Errorf("stored hash = %q, want the scheme prefix (verification dispatches on it)", stored)
	}
	if strings.Contains(stored, secret) {
		t.Error("the stored hash contains the secret")
	}
	if !h.Verify(stored, secret) {
		t.Error("Verify rejected the secret it just hashed")
	}
	if h.Verify(stored, "pk_abcdefgh_wrongwrongwrongwrongwrongwrongwro") {
		t.Error("Verify accepted a wrong secret")
	}
	// A different master key must not verify: the MAC is what binds the row to
	// this installation's key material.
	if NewKeyHasher([]byte("a different master key")).Verify(stored, secret) {
		t.Error("Verify accepted a hash built under another master key")
	}

	// Two hashes of the SAME secret differ (the salt), so a stolen table cannot
	// be compared against itself to spot duplicate keys.
	again, err := h.Hash(secret)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if again == stored {
		t.Error("two hashes of one secret are identical — the salt is not applied")
	}
	if !h.Verify(again, secret) {
		t.Error("Verify rejected the second hash of the same secret")
	}

	// A tampered MAC fails closed.
	tampered := stored[:len(stored)-2] + "AA"
	if h.Verify(tampered, secret) {
		t.Error("Verify accepted a tampered MAC")
	}
	// Anything malformed fails closed, including a row in the OTHER scheme —
	// dispatch happens one level up (VerifyClientKeyHash), not here.
	for _, bad := range []string{
		"",
		"secret",
		"$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA",
		"$hmacsha256$v=1$onlyonesegment",
		"$hmacsha256$v=2$c2FsdA$aGFzaA",
	} {
		if h.Verify(bad, secret) {
			t.Errorf("Verify accepted %q", bad)
		}
	}
}

func TestKeyHasher_KeyIDIsStableAndDistinct(t *testing.T) {
	a := NewKeyHasher([]byte("master-key-material-for-tests"))
	b := NewKeyHasher([]byte("master-key-material-for-tests"))
	c := NewKeyHasher([]byte("another masters key material"))

	if a.KeyID() != b.KeyID() {
		t.Error("KeyID is not stable for the same material")
	}
	if a.KeyID() == c.KeyID() {
		t.Error("KeyID collides across different material")
	}
	if strings.Contains(a.KeyID(), "master") {
		t.Error("KeyID leaks material")
	}
}

// The store's dispatch: a keyed-MAC row when it has the master-key-derived
// hasher, the legacy scheme when it does not, and a legacy row that keeps
// verifying — and is reported as legacy so it can be rewritten.
func TestStoreClientKeyHashing_DispatchAndLegacyUpgrade(t *testing.T) {
	s := openTokenStore(t)

	// No hasher attached: the legacy scheme (a store with no master key).
	legacy, err := s.HashClientKey("a-client-secret")
	if err != nil {
		t.Fatalf("HashClientKey: %v", err)
	}
	if !strings.HasPrefix(legacy, argon2HashPrefix) {
		t.Fatalf("hash without a master key = %q, want the legacy scheme", legacy)
	}

	s.SetKeyHasher(NewKeyHasher([]byte("master-key-material-for-tests")))

	mac, err := s.HashClientKey("a-client-secret")
	if err != nil {
		t.Fatalf("HashClientKey: %v", err)
	}
	if !strings.HasPrefix(mac, "$hmacsha256$v=1$") {
		t.Fatalf("hash with a master key = %q, want the keyed-MAC scheme", mac)
	}
	if ok, isLegacy := s.VerifyClientKeyHash(mac, "a-client-secret"); !ok || isLegacy {
		t.Errorf("MAC row verify = (%v, legacy %v), want (true, false)", ok, isLegacy)
	}
	if ok, _ := s.VerifyClientKeyHash(mac, "wrong"); ok {
		t.Error("MAC row verified a wrong secret")
	}

	// The legacy row still verifies, and says so — that flag is what triggers
	// the rewrite on the request path.
	if ok, isLegacy := s.VerifyClientKeyHash(legacy, "a-client-secret"); !ok || !isLegacy {
		t.Errorf("legacy row verify = (%v, legacy %v), want (true, true)", ok, isLegacy)
	}
	if ok, _ := s.VerifyClientKeyHash(legacy, "wrong"); ok {
		t.Error("legacy row verified a wrong secret")
	}
}

// A MAC row with no MAC key cannot be verified — fail closed, never open.
func TestStoreClientKeyHashing_MACRowWithoutKeyFailsClosed(t *testing.T) {
	s := openTokenStore(t)
	s.SetKeyHasher(NewKeyHasher([]byte("master-key-material-for-tests")))
	mac, err := s.HashClientKey("a-client-secret")
	if err != nil {
		t.Fatalf("HashClientKey: %v", err)
	}

	bare, err := Open(testCfg(t.TempDir() + "/bare.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = bare.Close() })

	if ok, _ := bare.VerifyClientKeyHash(mac, "a-client-secret"); ok {
		t.Error("a store without the MAC key verified a MAC row")
	}
}
