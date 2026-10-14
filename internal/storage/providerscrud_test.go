package storage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// Spec: schema — providers(id, type, name) with per-type config; the admin
// API needs full CRUD (the WebUI provider page creates,
// edits, lists, deletes). The `config` column carries the provider's
// NON-SECRET settings (base_url, model catalog options) as plain JSON —
// secrets NEVER live here, only in provider_tokens behind the AES-256-GCM
// envelope ("secrets never in config exports").

func TestProvider_GetAndList(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	if err := s.PutProvider(ctx, Provider{
		ID: "custom-1", Type: "custom", Name: "My OpenAI-compat",
		Config: json.RawMessage(`{"base_url":"https://api.example.com/v1"}`),
	}); err != nil {
		t.Fatalf("PutProvider: %v", err)
	}
	if err := s.PutProvider(ctx, Provider{ID: "ollama-1", Type: "ollama", Name: "Ollama Cloud"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetProvider(ctx, "custom-1")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if got.Type != "custom" || got.Name != "My OpenAI-compat" {
		t.Errorf("round-trip: got %+v", got)
	}
	if string(got.Config) != `{"base_url":"https://api.example.com/v1"}` {
		t.Errorf("config: got %s, want verbatim JSON", got.Config)
	}

	list, err := s.ListProviders(ctx)
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	// openTokenStore seeds codex-1; this test adds two more.
	if len(list) != 3 {
		t.Fatalf("list = %d, want 3", len(list))
	}
	// Stable order: the WebUI table must not reshuffle between requests.
	if list[0].ID != "codex-1" || list[1].ID != "custom-1" || list[2].ID != "ollama-1" {
		t.Errorf("list not id-sorted: %+v", list)
	}
}

func TestProvider_PutOverwritesAndRetypes(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	// Unlike EnsureProvider (which refuses silent re-typing for the seed
	// path), PutProvider IS the editor: an explicit WebUI save may change
	// type or name — upsert semantics, last write wins.
	if err := s.PutProvider(ctx, Provider{ID: "p", Type: "custom", Name: "One"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutProvider(ctx, Provider{ID: "p", Type: "ollama", Name: "Two"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetProvider(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "ollama" || got.Name != "Two" {
		t.Errorf("after overwrite: got %+v, want ollama/Two", got)
	}
}

func TestProvider_GetUnknownNotFound(t *testing.T) {
	s := openTokenStore(t)

	if _, err := s.GetProvider(context.Background(), "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown provider: got %v, want ErrNotFound", err)
	}
}

func TestProvider_DeleteCascadesInOneTransaction(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	// Deleting a provider removes its models and its (encrypted) tokens
	// atomically: no orphan rows, no dangling FK errors, and — critically —
	// no leftover credential material for a provider the operator removed.
	if err := s.PutProvider(ctx, Provider{ID: "p", Type: "custom", Name: "X"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutModel(ctx, Model{ID: "m", ProviderID: "p", Name: "model-x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutProviderTokens(ctx, ProviderToken{
		ProviderID: "p", Kind: "api_key", Secret: []byte("supersecret"),
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteProvider(ctx, "p"); err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}
	if _, err := s.GetProvider(ctx, "p"); !errors.Is(err, ErrNotFound) {
		t.Error("provider survived delete")
	}
	if _, err := s.GetModel(ctx, "m"); !errors.Is(err, ErrNotFound) {
		t.Error("model survived provider delete (orphan)")
	}
	if _, err := s.GetProviderToken(ctx, "p", "api_key"); !errors.Is(err, ErrNotFound) {
		t.Error("encrypted token survived provider delete (credential residue)")
	}
}

func TestProvider_DeleteUnknownNotFound(t *testing.T) {
	s := openTokenStore(t)

	// Typed-row deletes surface ErrNotFound (unlike config-KV deletes): a
	// delete the caller believed to exist but doesn't indicates a wiring
	// bug or a lost race — loud, not silent.
	if err := s.DeleteProvider(context.Background(), "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete unknown: got %v, want ErrNotFound", err)
	}
}

func TestProvider_ConfigRejectsSecretShapedKeys(t *testing.T) {
	s := openTokenStore(t)

	// Defense-in-depth for the no-secrets-in-config rule: if the config blob
	// already carries a secret-shaped key at the storage boundary, refuse it — a
	// WebUI bug must not be able to persist a plaintext credential even if
	// the API layer's validation were bypassed.
	err := s.PutProvider(context.Background(), Provider{
		ID: "p", Type: "custom", Name: "X",
		Config: json.RawMessage(`{"base_url":"https://x.example","api_key":"sk-live-secret"}`),
	})
	if err == nil {
		t.Error("secret-shaped config accepted")
	}
}
