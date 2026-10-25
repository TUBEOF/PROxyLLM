package webui

import (
	"context"
	"net/http"
	"testing"

	"github.com/tubeof/proxyllm/internal/storage"
)

// The wizard's status is recomputed from the live lists: a fresh instance
// needs setup, a configured one does not, and the persisted progress never
// overrides the counts.
func TestOnboarding_StatusAndProgress(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")
	ctx := context.Background()

	get := func() onboardingResponseDTO {
		t.Helper()
		w := e.authedCall(t, http.MethodGet, "/api/admin/onboarding", "", s)
		assertStatus(t, w, http.StatusOK, "onboarding status")
		var got onboardingResponseDTO
		decodeJSON(t, w, &got)
		return got
	}

	fresh := get()
	if !fresh.NeedsSetup {
		t.Fatal("a fresh instance must need setup")
	}
	if fresh.Counts.Providers != 0 || fresh.Counts.Models != 0 || fresh.Counts.ClientKeys != 0 {
		t.Fatalf("fresh counts = %+v, want zero", fresh.Counts)
	}

	if err := e.api.store.PutProvider(ctx, storage.Provider{
		ID: "acc1", Type: "ollama-cloud", Name: "Ollama",
	}); err != nil {
		t.Fatalf("provider: %v", err)
	}
	if err := e.api.store.PutModel(ctx, storage.Model{
		ID: "ollama-cloud/model", ProviderID: "acc1", Name: "model",
		Origin: storage.ModelSyncedOrigin,
	}); err != nil {
		t.Fatalf("model: %v", err)
	}
	if w := e.authedCall(t, http.MethodPost, "/api/admin/keys", `{"name":"setup"}`, s); w.Code != http.StatusOK {
		t.Fatalf("key: HTTP %d", w.Code)
	}

	configured := get()
	if configured.NeedsSetup {
		t.Fatalf("a configured instance must not need setup: %+v", configured)
	}
	if configured.Counts.LLMProviders != 1 || configured.Counts.Models != 1 || configured.Counts.ClientKeys != 1 {
		t.Fatalf("counts = %+v, want 1 llm / 1 model / 1 key", configured.Counts)
	}

	w := e.authedCall(t, http.MethodPut, "/api/admin/onboarding",
		`{"completed":["provider","model","web_tools","fallbacks","key"],"dismissed":false}`, s)
	assertStatus(t, w, http.StatusOK, "onboarding put")
	progress := get()
	if len(progress.State.Completed) != 5 || progress.State.Version != storage.OnboardingStateVersion {
		t.Fatalf("state = %+v, want the five completed steps", progress.State)
	}

	w = e.authedCall(t, http.MethodPut, "/api/admin/onboarding",
		`{"completed":["not-a-step"]}`, s)
	assertStatus(t, w, http.StatusBadRequest, "unknown step")

	w = e.authedCall(t, http.MethodPut, "/api/admin/onboarding",
		`{"dismissed":true}`, s)
	assertStatus(t, w, http.StatusOK, "dismiss")
	if get().NeedsSetup {
		t.Fatal("a dismissed wizard must not reopen on its own")
	}
}
