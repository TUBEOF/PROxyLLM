package storage

import (
	"context"
	"testing"
)

// Spec: config layering — "defaults → config file → ENV → WebUI/DB
// overrides (highest, exportable as config file)". The config table is the
// DB-override layer the settings page reads and writes (the global
// non-Claude switch anthropic_surface_non_claude lives here,
// as do retention/auth settings the WebUI exposes).

func TestConfigValue_RoundTrip(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	if err := s.SetConfigValue(ctx, "anthropic_surface_non_claude", "warn"); err != nil {
		t.Fatalf("SetConfigValue: %v", err)
	}
	v, ok, err := s.GetConfigValue(ctx, "anthropic_surface_non_claude")
	if err != nil {
		t.Fatalf("GetConfigValue: %v", err)
	}
	if !ok || v != "warn" {
		t.Errorf("round-trip: got (%q, %t), want (warn, true)", v, ok)
	}
}

func TestConfigValue_MissingIsNotAnError(t *testing.T) {
	s := openTokenStore(t)

	// An absent override is the normal state — most settings have no DB
	// override and fall through to the file/ENV/default layers. ok=false,
	// never an error, so callers cannot conflate "unset" with "broken".
	v, ok, err := s.GetConfigValue(context.Background(), "no.such.key")
	if err != nil {
		t.Fatalf("GetConfigValue on missing key: %v", err)
	}
	if ok || v != "" {
		t.Errorf("missing key: got (%q, %t), want (\"\", false)", v, ok)
	}
}

func TestConfigValue_SetOverwrites(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	if err := s.SetConfigValue(ctx, "k", "first"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetConfigValue(ctx, "k", "second"); err != nil {
		t.Fatal(err)
	}
	v, _, err := s.GetConfigValue(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if v != "second" {
		t.Errorf("overwrite: got %q, want second (upsert, not duplicate-row error)", v)
	}
}

func TestConfigValue_DeleteIdempotent(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	// Deleting an override is state reconciliation toward "fall through to
	// the lower layers" — the target state is achieved whether or not the
	// row existed, so a second delete must not error (unlike the typed-row
	// deletes, which surface ErrNotFound to catch wiring bugs).
	if err := s.DeleteConfigValue(ctx, "k"); err != nil {
		t.Fatalf("delete of missing override: %v (want nil)", err)
	}
	if err := s.SetConfigValue(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteConfigValue(ctx, "k"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteConfigValue(ctx, "k"); err != nil {
		t.Fatalf("second delete: %v (want idempotent nil)", err)
	}
	if _, ok, _ := s.GetConfigValue(ctx, "k"); ok {
		t.Error("value still readable after delete")
	}
}

func TestConfigValue_ListSorted(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	for _, kv := range [][2]string{
		{"b.second", "2"}, {"a.first", "1"}, {"c.third", "3"},
	} {
		if err := s.SetConfigValue(ctx, kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.ListConfigValues(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	// Deterministic key order: the settings page renders a stable list, not
	// whatever the physical row order happens to be.
	if entries[0].Key != "a.first" || entries[1].Key != "b.second" || entries[2].Key != "c.third" {
		t.Errorf("entries not key-sorted: %+v", entries)
	}
}
