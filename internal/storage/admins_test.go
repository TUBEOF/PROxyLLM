package storage

import (
	"context"
	"errors"
	"testing"
)

// Spec: plan Analysis C schema (webui_admins: id, username UNIQUE,
// password_hash) + Analysis G control 1 (WebUI login, Argon2id) + research
// S-8 (privilege changes renew sessions — the session layer needs the admin
// row id). The password hash is opaque to this layer: the auth layer derives
// it via Argon2Hasher; storage only persists and resolves it.

func TestAdmins_CountZeroOnFreshStore(t *testing.T) {
	s := openTokenStore(t)

	// First-run bootstrap: the WebUI setup flow shows "create the first
	// admin" iff the table is empty — the count is the signal.
	n, err := s.CountAdmins(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("fresh store admin count = %d, want 0", n)
	}
}

func TestAdmins_CreateAndLookup(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	if err := s.CreateAdmin(ctx, "adm-1", "operator", "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA"); err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}
	got, err := s.GetAdminByUsername(ctx, "operator")
	if err != nil {
		t.Fatalf("GetAdminByUsername: %v", err)
	}
	if got.ID != "adm-1" || got.Username != "operator" {
		t.Errorf("round-trip: got %+v, want adm-1/operator", got)
	}
	if got.PasswordHash == "" {
		t.Error("password hash empty — login verification would have nothing to compare")
	}
}

func TestAdmins_DuplicateUsernameRejected(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	if err := s.CreateAdmin(ctx, "adm-1", "operator", "h1"); err != nil {
		t.Fatal(err)
	}
	// The UNIQUE constraint is the integrity guarantee; a second admin with
	// the same username must be an error, never a silent second row.
	if err := s.CreateAdmin(ctx, "adm-2", "operator", "h2"); err == nil {
		t.Error("duplicate username accepted")
	}
}

func TestAdmins_UnknownUsernameNotFound(t *testing.T) {
	s := openTokenStore(t)

	// Login failure must be indistinguishable in shape from success paths:
	// a single ErrNotFound for "no such user" — the auth layer maps it to a
	// generic 401 and never reveals whether the username exists.
	_, err := s.GetAdminByUsername(context.Background(), "nobody")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("lookup of unknown username: got %v, want ErrNotFound", err)
	}
}

func TestAdmins_UnknownUsernameNotAdminErrNotFound(t *testing.T) {
	// The 401-body doctrine (never distinguish causes) is auth-layer
	// policy; storage-layer policy pinned here: BOTH "no such user" and
	// "no such id" surface ErrNotFound so callers cannot special-case.
	s := openTokenStore(t)
	if err := s.UpdateAdminPassword(context.Background(), "no-such-id", "h2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateAdminPassword unknown id: got %v, want ErrNotFound", err)
	}
}

func TestAdmins_UpdatePassword(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	if err := s.CreateAdmin(ctx, "adm-1", "operator", "old-hash"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAdminPassword(ctx, "adm-1", "new-hash"); err != nil {
		t.Fatalf("UpdateAdminPassword: %v", err)
	}
	got, err := s.GetAdminByUsername(ctx, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if got.PasswordHash != "new-hash" {
		t.Errorf("password hash = %q, want new-hash (replaced, not appended)", got.PasswordHash)
	}
}
