package storage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// Spec: plan Analysis C schema (models: id, provider_id, name, capabilities
// JSON, compat JSON, priority) + §6.4 (per-model compat policy lives in
// models.compat, WebUI-editable via the "Models & Compatibility" area).

func seedProvider(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO providers (id, type, name) VALUES (?, 'codex', ?)`, id, id); err != nil {
		t.Fatalf("seed provider %s: %v", id, err)
	}
}

func TestModel_RoundTrip(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	in := Model{
		ID:           "gpt-5-codex",
		ProviderID:   "codex-1",
		Name:         "GPT-5 Codex",
		Capabilities: json.RawMessage(`{"streaming":true,"tools":true}`),
		Compat:       json.RawMessage(`{"thinking_handling":"strip"}`),
		Priority:     7,
	}
	if err := s.PutModel(ctx, in); err != nil {
		t.Fatalf("PutModel: %v", err)
	}

	got, err := s.GetModel(ctx, "gpt-5-codex")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != in.ID || got.ProviderID != in.ProviderID || got.Name != in.Name ||
		string(got.Capabilities) != string(in.Capabilities) ||
		string(got.Compat) != string(in.Compat) || got.Priority != in.Priority {
		t.Errorf("round-trip mismatch: got %+v want %+v", got, in)
	}
}

func TestModel_MissingIsErrNotFound(t *testing.T) {
	s := openTokenStore(t)
	if _, err := s.GetModel(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing model error = %v, want ErrNotFound", err)
	}
}

func TestModel_UpsertOverwrites(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	if err := s.PutModel(ctx, Model{ID: "m1", ProviderID: "codex-1", Name: "one"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutModel(ctx, Model{ID: "m1", ProviderID: "codex-1", Name: "two", Priority: 3}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetModel(ctx, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "two" || got.Priority != 3 {
		t.Errorf("upsert did not overwrite: %+v", got)
	}
}

func TestModel_ListByProviderAndAll(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()
	seedProvider(t, s, "ollama-1")

	for _, m := range []Model{
		{ID: "m1", ProviderID: "codex-1", Name: "a"},
		{ID: "m2", ProviderID: "codex-1", Name: "b", Priority: 2},
		{ID: "m3", ProviderID: "ollama-1", Name: "c"},
	} {
		if err := s.PutModel(ctx, m); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("ListModels = %d rows, want 3", len(all))
	}

	forProvider, err := s.ListModelsByProvider(ctx, "codex-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(forProvider) != 2 {
		t.Fatalf("ListModelsByProvider(codex-1) = %d rows, want 2", len(forProvider))
	}
	for _, m := range forProvider {
		if m.ProviderID != "codex-1" {
			t.Errorf("provider filter leak: %+v", m)
		}
	}
}

func TestModel_Delete(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	if err := s.PutModel(ctx, Model{ID: "m1", ProviderID: "codex-1", Name: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteModel(ctx, "m1"); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}
	if err := s.DeleteModel(ctx, "m1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting missing model = %v, want ErrNotFound", err)
	}
}

func TestModel_RejectsUnknownProvider(t *testing.T) {
	s := openTokenStore(t)

	err := s.PutModel(context.Background(), Model{ID: "m1", ProviderID: "ghost", Name: "a"})
	if err == nil {
		t.Error("model with unknown provider must fail (FK violation)")
	}
}
