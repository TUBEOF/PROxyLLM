package storage

import (
	"context"
	"testing"
)

// Spec: schema (providers: id, type, name) — the FK target for models and
// provider_tokens. EnsureProvider is the exported seeding path the httpapi
// harness and the WebUI provider CRUD build on:
// idempotent upsert (ON CONFLICT), never silently renaming an existing
// provider of a different type.

// 1. Round trip: EnsureProvider inserts the row; a second call with the
// same shape succeeds without duplicating (idempotent), and a model can
// then reference the provider (FK satisfied).
func TestEnsureProvider_RoundTrip(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	if err := s.EnsureProvider(ctx, "mock-main", "custom", "Mock Main"); err != nil {
		t.Fatalf("EnsureProvider: %v", err)
	}
	if err := s.EnsureProvider(ctx, "mock-main", "custom", "Mock Main"); err != nil {
		t.Fatalf("EnsureProvider (idempotent second call): %v", err)
	}

	var typ, name string
	err := s.db.QueryRow(`SELECT type, name FROM providers WHERE id = ?`, "mock-main").Scan(&typ, &name)
	if err != nil {
		t.Fatalf("select provider: %v", err)
	}
	if typ != "custom" || name != "Mock Main" {
		t.Errorf("provider row = %s/%s, want custom/Mock Main", typ, name)
	}
}

// 2. Validation: empty id or empty type rejected — never persist an
// addressless row.
func TestEnsureProvider_Validation(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	if err := s.EnsureProvider(ctx, "", "custom", "x"); err == nil {
		t.Error("empty id accepted")
	}
	if err := s.EnsureProvider(ctx, "p1", "", "x"); err == nil {
		t.Error("empty type accepted")
	}
}

// 3. Type conflict: EnsureProvider must never silently re-type an
// existing provider — a conflicting type is an error (the WebUI provider
// editor decides how to resolve, not the storage layer).
func TestEnsureProvider_TypeConflict(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	if err := s.EnsureProvider(ctx, "mock-main", "custom", "Mock Main"); err != nil {
		t.Fatalf("EnsureProvider: %v", err)
	}
	if err := s.EnsureProvider(ctx, "mock-main", "codex", "Other"); err == nil {
		t.Fatal("re-typing an existing provider accepted — silent type change")
	}
	// The original row is untouched.
	var typ, name string
	if err := s.db.QueryRow(`SELECT type, name FROM providers WHERE id = ?`, "mock-main").Scan(&typ, &name); err != nil {
		t.Fatal(err)
	}
	if typ != "custom" || name != "Mock Main" {
		t.Errorf("provider row mutated on conflict: %s/%s", typ, name)
	}
}

// 4. Name update: a second call with the same type but a new name updates
// the display name (idempotent re-sync from provider discovery).
func TestEnsureProvider_NameUpdate(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	if err := s.EnsureProvider(ctx, "mock-main", "custom", "Old Name"); err != nil {
		t.Fatalf("EnsureProvider: %v", err)
	}
	if err := s.EnsureProvider(ctx, "mock-main", "custom", "New Name"); err != nil {
		t.Fatalf("EnsureProvider (name update): %v", err)
	}
	var name string
	if err := s.db.QueryRow(`SELECT name FROM providers WHERE id = ?`, "mock-main").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "New Name" {
		t.Errorf("name = %q, want New Name", name)
	}
}
