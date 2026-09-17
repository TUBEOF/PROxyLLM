package storage

import (
	"context"
	"errors"
	"testing"
)

// Spec: ADR-010 — model rows record their provenance (models.origin:
// "manual" | "synced"), the sync deletes only synced rows absent from the
// upstream list (operator rows survive), and the capability fallback lists
// ride the config-KV with last-known-good read semantics.

func TestModel_OriginDefaultsToManual(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	// openTokenStore seeds the "codex-1" provider row.
	if err := s.PutModel(ctx, Model{ID: "m1", ProviderID: "codex-1", Name: "one"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetModel(ctx, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != ModelManualOrigin {
		t.Errorf("origin = %q, want %q (the empty origin is the operator path)", got.Origin, ModelManualOrigin)
	}
}

func TestModel_SyncedOriginRoundTrip(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	if err := s.PutModel(ctx, Model{ID: "m1", ProviderID: "codex-1", Name: "one", Origin: ModelSyncedOrigin}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetModel(ctx, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != ModelSyncedOrigin {
		t.Errorf("origin = %q, want %q", got.Origin, ModelSyncedOrigin)
	}
}

func TestDeleteModelsNotIn_RemovesOnlySyncedMissing(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	if err := s.PutModel(ctx, Model{ID: "kept-synced", ProviderID: "codex-1", Name: "a", Origin: ModelSyncedOrigin}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutModel(ctx, Model{ID: "dropped-synced", ProviderID: "codex-1", Name: "b", Origin: ModelSyncedOrigin}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutModel(ctx, Model{ID: "manual-row", ProviderID: "codex-1", Name: "c", Origin: ModelManualOrigin}); err != nil {
		t.Fatal(err)
	}
	// A second provider for the cross-provider check.
	if _, err := s.db.Exec(`INSERT INTO providers (id, type, name) VALUES ('p2', 'codex', 'p2')`); err != nil && !isUniqueViolation(err) {
		t.Fatalf("seed p2: %v", err)
	}
	if err := s.PutModel(ctx, Model{ID: "other-provider", ProviderID: "p2", Name: "d", Origin: ModelSyncedOrigin}); err != nil {
		t.Fatal(err)
	}

	// The upstream still lists kept-synced; dropped-synced vanished.
	if err := s.DeleteModelsNotIn(ctx, "codex-1", []string{"kept-synced"}, ModelSyncedOrigin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetModel(ctx, "kept-synced"); err != nil {
		t.Errorf("kept-synced should survive: %v", err)
	}
	if _, err := s.GetModel(ctx, "dropped-synced"); !errors.Is(err, ErrNotFound) {
		t.Errorf("dropped-synced should be gone, got %v", err)
	}
	if _, err := s.GetModel(ctx, "manual-row"); err != nil {
		t.Errorf("manual-row must survive the sync delete (ADR-010 policy): %v", err)
	}
	if _, err := s.GetModel(ctx, "other-provider"); err != nil {
		t.Errorf("other-provider (different provider row) should survive: %v", err)
	}
}

// containsString reports whether s contains sub (tiny helper for driver
// error text matching in seeds).
func containsString(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func isUniqueViolation(err error) bool { return err != nil && containsString(err.Error(), "UNIQUE") }

func TestDeleteModelsNotIn_RefusesManualOrigin(t *testing.T) {
	s := openTokenStore(t)
	if err := s.DeleteModelsNotIn(context.Background(), "p1", nil, ModelManualOrigin); !errors.Is(err, ErrInvalid) {
		t.Errorf("manual-origin bulk delete = %v, want ErrInvalid (the sync must never wipe operator rows)", err)
	}
	if err := s.DeleteModelsNotIn(context.Background(), "p1", nil, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty-origin bulk delete = %v, want ErrInvalid", err)
	}
}

func TestFallbackLists_RoundTrip(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	if err := s.PutVisionFallback(ctx, []string{"acc-a", "custom-openai"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutWebToolFallbacks(ctx, WebToolFallbacks{Search: []string{"acc-fc"}, Fetch: []string{"acc-fc2", "acc-fc"}}); err != nil {
		t.Fatal(err)
	}

	vision, err := s.GetVisionFallback(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(vision) != 2 || vision[0] != "acc-a" || vision[1] != "custom-openai" {
		t.Errorf("vision round-trip: %v", vision)
	}
	wf, err := s.GetWebToolFallbacks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(wf.Search) != 1 || wf.Search[0] != "acc-fc" {
		t.Errorf("web_search round-trip: %v", wf.Search)
	}
	if len(wf.Fetch) != 2 || wf.Fetch[0] != "acc-fc2" || wf.Fetch[1] != "acc-fc" {
		t.Errorf("web_fetch round-trip: %v", wf.Fetch)
	}

	// Clearing stores an explicit empty list, not an absent key.
	if err := s.PutVisionFallback(ctx, nil); err != nil {
		t.Fatal(err)
	}
	vision, err = s.GetVisionFallback(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if vision == nil || len(vision) != 0 {
		t.Errorf("cleared vision list = %v, want empty non-nil", vision)
	}

	// Unset reads degrade to nil.
	s2 := openTokenStore(t)
	v2, err := s2.GetVisionFallback(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v2 != nil {
		t.Errorf("unset vision list = %v, want nil", v2)
	}
}

func TestModelSyncConfig_RoundTrip(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	// Defaults: enabled, 24h.
	enabled, err := s.GetModelSyncEnabled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Error("model sync must default to enabled")
	}
	hours, err := s.GetModelSyncIntervalHours(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hours != DefaultModelSyncIntervalHours {
		t.Errorf("default interval = %d, want %d", hours, DefaultModelSyncIntervalHours)
	}

	if err := s.SetModelSyncEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetModelSyncIntervalHours(ctx, 6); err != nil {
		t.Fatal(err)
	}
	enabled, err = s.GetModelSyncEnabled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Error("disabled toggle did not persist")
	}
	hours, err = s.GetModelSyncIntervalHours(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hours != 6 {
		t.Errorf("interval = %d, want 6", hours)
	}

	if err := s.SetModelSyncIntervalHours(ctx, 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("interval 0 = %v, want ErrInvalid (the scheduler ticker panics on non-positive)", err)
	}
}

func TestModelSyncState_RoundTrip(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	// Unset reads as the zero value (never ran), never an error.
	st, err := s.GetModelSyncState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.RanAt != 0 || len(st.Providers) != 0 {
		t.Errorf("unset state = %+v, want zero", st)
	}

	in := ModelSyncState{RanAt: 12345, Providers: []ModelSyncProviderState{
		{ProviderID: "acc-1", Added: 3, Updated: 1, Removed: 2},
		{ProviderID: "acc-2", Skipped: 1, SkippedReason: "upstream 500"},
	}}
	if err := s.PutModelSyncState(ctx, in); err != nil {
		t.Fatal(err)
	}
	st, err = s.GetModelSyncState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.RanAt != 12345 || len(st.Providers) != 2 || st.Providers[0].Added != 3 || st.Providers[1].SkippedReason != "upstream 500" {
		t.Errorf("state round-trip mismatch: %+v", st)
	}
}
