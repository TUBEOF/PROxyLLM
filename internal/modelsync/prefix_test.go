package modelsync

// Prefix contract tests (ADR-010 extension, model identity): a synced row's
// ID is "<model_prefix>/<upstream-name>" — the prefix comes from the
// provider type's shared settings (default: the type string), the Name
// column carries the bare upstream wire name. Spec source: user decision
// 2026-09-21 (model identity "provider/model", editable per type).

import (
	"context"
	"testing"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/storage"
)

// putTypeSettings seeds one type's shared settings blob.
func putTypeSettings(t *testing.T, s *storage.Store, typ, blob string) {
	t.Helper()
	settings, err := s.GetProviderTypeSettings(context.Background())
	if err != nil {
		t.Fatalf("GetProviderTypeSettings: %v", err)
	}
	settings[typ] = []byte(blob)
	if err := s.PutProviderTypeSettings(context.Background(), settings); err != nil {
		t.Fatalf("PutProviderTypeSettings: %v", err)
	}
}

func TestSync_PrefixesImportedModelIDs(t *testing.T) {
	s := openStore(t, "acc-a")
	putTypeSettings(t, s, "custom-openai", `{"model_prefix":"myhost"}`)
	reg := &fakeRegistry{}
	reg.Register((&stubProvider{id: "acc-a", list: []provider.ModelInfo{
		{Name: "glm-5.3"}, {Name: "llama-4"},
	}}).asLister())

	if _, err := New(s, reg, testLogger()).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The ID carries the configured prefix; the Name is the bare wire name.
	m := mustGet(t, s, "myhost/glm-5.3")
	if m.ProviderID != "acc-a" || m.Name != "glm-5.3" || m.Origin != storage.ModelSyncedOrigin {
		t.Errorf("imported model mismatch: %+v", m)
	}
	if _, err := s.GetModel(context.Background(), "llama-4"); err == nil {
		t.Error("unprefixed id must not exist")
	}
	if _, err := s.GetModel(context.Background(), "myhost/llama-4"); err != nil {
		t.Errorf("myhost/llama-4: %v", err)
	}
}

func TestSync_DefaultPrefixIsProviderType(t *testing.T) {
	s := openStore(t, "acc-a") // seeds type custom-openai, no settings
	reg := &fakeRegistry{}
	reg.Register((&stubProvider{id: "acc-a", list: []provider.ModelInfo{{Name: "model-x"}}}).asLister())

	if _, err := New(s, reg, testLogger()).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := s.GetModel(context.Background(), "custom-openai/model-x"); err != nil {
		t.Fatalf("default prefix (type string) missing: %v", err)
	}
	if m := mustGet(t, s, "custom-openai/model-x"); m.Name != "model-x" {
		t.Errorf("wire name = %q, want model-x", m.Name)
	}
}

func TestSync_PrefixChangeReimportsUnderNewID(t *testing.T) {
	// The prefix is part of the identity: renaming it means the old prefixed
	// row is upstream-dropped (synced origin) and the model re-imports under
	// the new ID. Operator rows survive as always.
	s := openStore(t, "acc-a")
	reg := &fakeRegistry{}
	reg.Register((&stubProvider{id: "acc-a", list: []provider.ModelInfo{{Name: "model-x"}}}).asLister())

	if _, err := New(s, reg, testLogger()).Run(context.Background()); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	if _, err := s.GetModel(context.Background(), "custom-openai/model-x"); err != nil {
		t.Fatalf("first sync missing prefixed row: %v", err)
	}

	putTypeSettings(t, s, "custom-openai", `{"model_prefix":"renamed"}`)
	if _, err := New(s, reg, testLogger()).Run(context.Background()); err != nil {
		t.Fatalf("Run 2: %v", err)
	}

	if _, err := s.GetModel(context.Background(), "renamed/model-x"); err != nil {
		t.Fatalf("re-sync under new prefix missing: %v", err)
	}
	if _, err := s.GetModel(context.Background(), "custom-openai/model-x"); !isNotFound(err) {
		t.Errorf("old prefixed synced row must be dropped, got %v", err)
	}
}
