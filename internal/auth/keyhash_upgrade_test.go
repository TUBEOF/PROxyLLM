package auth

// Client-key hashing on the request path.
//
// Spec: a 256-bit client secret does not need Argon2id's brute-force resistance
// (storage/keyhash.go), but a key that was already issued must keep working —
// so a legacy row verifies and is rewritten in place on its first successful
// use, with the presented secret in hand. What must never happen: the raw secret
// reaching the database, or a valid key being rejected because the upgrade
// failed.

import (
	"context"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/storage"
)

func TestCreateClientKey_UsesTheKeyedMacWhenTheStoreHasAMasterKey(t *testing.T) {
	st := newTestStore(t)
	st.SetKeyHasher(storage.NewKeyHasher([]byte("master-key-material-for-tests")))

	id, key, err := CreateClientKey(context.Background(), st, "hmac-key")
	if err != nil {
		t.Fatalf("CreateClientKey: %v", err)
	}
	prefix, secret, ok := splitClientKey(key)
	if !ok {
		t.Fatalf("generated key does not parse: %q", key)
	}

	rows, err := st.ListClientKeysByPrefix(context.Background(), prefix)
	if err != nil {
		t.Fatalf("ListClientKeysByPrefix: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("rows = %+v, want the key just created", rows)
	}
	if !strings.HasPrefix(rows[0].KeyHash, "$hmacsha256$v=1$") {
		t.Errorf("stored hash = %q, want the keyed-MAC scheme", rows[0].KeyHash)
	}
	// The one thing that must never happen: the secret itself in the row.
	if strings.Contains(rows[0].KeyHash, secret) {
		t.Error("the stored row contains the raw secret")
	}
}

// A row written before this change verifies and is migrated on first use — the
// upgrade path an existing installation depends on, since the secrets are
// display-once and cannot be reissued.
func TestVerifyKey_UpgradesALegacyRowOnFirstUse(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	// Written with no master key attached: the legacy Argon2id scheme.
	id, key, err := CreateClientKey(ctx, st, "legacy-key")
	if err != nil {
		t.Fatalf("CreateClientKey: %v", err)
	}
	prefix, _, _ := splitClientKey(key)
	before, err := st.ListClientKeysByPrefix(ctx, prefix)
	if err != nil || len(before) != 1 {
		t.Fatalf("rows before = %+v (%v)", before, err)
	}
	if !strings.HasPrefix(before[0].KeyHash, "$argon2id$") {
		t.Fatalf("seed hash = %q, want the legacy scheme", before[0].KeyHash)
	}

	// Now the master key arrives (a restart after the upgrade), and the key is
	// used.
	st.SetKeyHasher(storage.NewKeyHasher([]byte("master-key-material-for-tests")))
	if got := verifyKey(ctx, st, key); got.id != id {
		t.Fatalf("verifyKey = %+v, want the legacy key to keep working (id %s)", got, id)
	}

	after, err := st.ListClientKeysByPrefix(ctx, prefix)
	if err != nil || len(after) != 1 {
		t.Fatalf("rows after = %+v (%v)", after, err)
	}
	if !strings.HasPrefix(after[0].KeyHash, "$hmacsha256$v=1$") {
		t.Errorf("hash after first use = %q, want the migration to the keyed-MAC scheme", after[0].KeyHash)
	}
	// Still the same key, now on the fast path.
	if got := verifyKey(ctx, st, key); got.id != id {
		t.Errorf("verifyKey after migration = %+v, want the same row", got)
	}
	// And a wrong key is still rejected.
	if got := verifyKey(ctx, st, key[:len(key)-1]+"X"); got.id != "" {
		t.Error("verifyKey accepted a wrong key")
	}
}

// The upgrade is best-effort by construction: verifyKey returns the row id
// BEFORE the rewrite and DISCARDS the rewrite's error (see its comment), so a
// store that cannot rewrite the row still authenticates the key — the same
// posture the last_used_at touch already had. There is deliberately no test for
// "the rewrite fails but auth succeeds": the only way to fail the rewrite alone
// would be a store whose writes fail while its reads succeed, and inventing a
// seam for that would test the seam, not this property.
