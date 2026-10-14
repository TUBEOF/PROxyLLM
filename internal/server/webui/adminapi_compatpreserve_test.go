package webui

// The SPA no longer sends `compat` on model saves
// (the knobs are internal) — a plain UPSERT would then wipe the
// stored blob on EVERY operator save. The contract: an absent compat
// field PRESERVES the stored blob (legacy read-only override); an
// EXPLICIT compat field still replaces it (the field stays the wire
// contract, and null/cleared blobs remain expressible for API users).

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tubeof/proxyllm/internal/storage"
)

func TestModels_PutWithoutCompatPreservesBlob(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")
	ctx := context.Background()
	if err := e.st.EnsureProvider(ctx, "prov-a", "ollama-cloud", "A"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := e.st.PutModel(ctx, storage.Model{
		ID:         "ollamacloud/m",
		ProviderID: "prov-a",
		Name:       "m",
		Compat:     json.RawMessage(`{"cache_control":"forward","max_tokens_clamp":32000}`),
	}); err != nil {
		t.Fatalf("seed model: %v", err)
	}

	// The SPA save: NO compat field at all.
	w := e.authedCall(t, http.MethodPut, "/api/admin/models/ollamacloud/m",
		`{"provider_id":"prov-a","name":"m","capabilities":{"vision":true},"priority":5}`, s)
	assertStatus(t, w, http.StatusOK, "put model without compat")

	row, err := e.st.GetModel(ctx, "ollamacloud/m")
	if err != nil {
		t.Fatalf("get model: %v", err)
	}
	var compat map[string]any
	if err := json.Unmarshal(row.Compat, &compat); err != nil {
		t.Fatalf("compat blob: %v", err)
	}
	if compat["cache_control"] != "forward" || compat["max_tokens_clamp"] != float64(32000) {
		t.Fatalf("stored compat blob was wiped by a compat-less save: %s", row.Compat)
	}

	// An EXPLICIT compat still replaces (the wire contract for whoever
	// sends it — server-side tooling).
	w = e.authedCall(t, http.MethodPut, "/api/admin/models/ollamacloud/m",
		`{"provider_id":"prov-a","name":"m","capabilities":{"vision":true},"priority":5,"compat":{"thinking_handling":"block"}}`, s)
	assertStatus(t, w, http.StatusOK, "put model with compat")
	row, err = e.st.GetModel(ctx, "ollamacloud/m")
	if err != nil {
		t.Fatalf("get model: %v", err)
	}
	if string(row.Compat) != `{"thinking_handling":"block"}` {
		t.Fatalf("explicit compat lost: %s", row.Compat)
	}
}
