package storage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// Spec: automatic capabilities (ADR-010) — a real
// upstream capability-gap rejection LEARNS the missing capability into the
// model row's capabilities blob (vision=false), located by the upstream pair
// (provider + wire name). A pinned flag (the blob's "pinned" object, set by
// the operator's manual selection) is never overwritten by learning, and
// learning must not rewrite the row's origin (synced rows stay synced so
// the sync's delete policy keeps applying).

// seedModelWithCaps upserts one model row with a capabilities blob.
func seedModelWithCaps(t *testing.T, s *Store, m Model) {
	t.Helper()
	if err := s.PutModel(context.Background(), m); err != nil {
		t.Fatalf("PutModel %s: %v", m.ID, err)
	}
}

func TestMergeLearnedCapability_WritesVisionFalse(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	seedModelWithCaps(t, s, Model{
		ID: "ollamacloud/llama", ProviderID: "codex-1", Name: "llama",
		Capabilities: json.RawMessage(`{"streaming":true}`),
	})

	if err := s.MergeLearnedCapability(ctx, "codex-1", "llama", "vision", false); err != nil {
		t.Fatalf("MergeLearnedCapability: %v", err)
	}

	got, err := s.GetModel(ctx, "ollamacloud/llama")
	if err != nil {
		t.Fatal(err)
	}
	var caps map[string]any
	if err := json.Unmarshal(got.Capabilities, &caps); err != nil {
		t.Fatalf("caps blob: %v", err)
	}
	if v, ok := caps["vision"].(bool); !ok || v {
		t.Errorf("caps = %s, want vision:false learned", got.Capabilities)
	}
	if v, ok := caps["streaming"].(bool); !ok || !v {
		t.Errorf("caps = %s, want streaming:true preserved", got.Capabilities)
	}
}

func TestMergeLearnedCapability_PinnedFlagWins(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	seedModelWithCaps(t, s, Model{
		ID: "ollamacloud/llama", ProviderID: "codex-1", Name: "llama",
		Capabilities: json.RawMessage(`{"vision":true,"pinned":{"vision":true}}`),
	})

	if err := s.MergeLearnedCapability(ctx, "codex-1", "llama", "vision", false); err != nil {
		t.Fatalf("MergeLearnedCapability: %v", err)
	}

	got, err := s.GetModel(ctx, "ollamacloud/llama")
	if err != nil {
		t.Fatal(err)
	}
	var caps map[string]any
	if err := json.Unmarshal(got.Capabilities, &caps); err != nil {
		t.Fatal(err)
	}
	if v, ok := caps["vision"].(bool); !ok || !v {
		t.Errorf("caps = %s, want vision:true (pinned — learning must not touch it)", got.Capabilities)
	}
}

func TestMergeLearnedCapability_KeepsSyncedOrigin(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	seedModelWithCaps(t, s, Model{
		ID: "ollamacloud/llama", ProviderID: "codex-1", Name: "llama",
		Capabilities: json.RawMessage(`{}`), Origin: ModelSyncedOrigin,
	})

	if err := s.MergeLearnedCapability(ctx, "codex-1", "llama", "vision", false); err != nil {
		t.Fatalf("MergeLearnedCapability: %v", err)
	}

	got, err := s.GetModel(ctx, "ollamacloud/llama")
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != ModelSyncedOrigin {
		t.Errorf("origin = %q, want %q (learning must not flip sync semantics)", got.Origin, ModelSyncedOrigin)
	}
}

func TestMergeLearnedCapability_UnknownRowIsNoOp(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	// No row for this pair — learning is opportunistic, never an error.
	if err := s.MergeLearnedCapability(ctx, "codex-1", "ghost-model", "vision", false); err != nil {
		t.Errorf("unknown row: err = %v, want nil (no-op)", err)
	}
	if _, err := s.GetModel(ctx, "ghost-model"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a no-op must not create rows: %v", err)
	}
}

func TestGetModelByProviderAndName_LegacyRowMatches(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	// Legacy unprefixed row: name == id.
	seedModelWithCaps(t, s, Model{ID: "llama", ProviderID: "codex-1", Name: "llama"})

	got, err := s.GetModelByProviderAndName(ctx, "codex-1", "llama")
	if err != nil {
		t.Fatalf("GetModelByProviderAndName: %v", err)
	}
	if got.ID != "llama" {
		t.Errorf("row = %s, want the legacy row", got.ID)
	}
}
