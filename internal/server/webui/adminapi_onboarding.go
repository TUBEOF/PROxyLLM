package webui

// First-run onboarding (the setup wizard): a read-only status endpoint that
// recomputes "is this instance configured?" from the live lists, plus a typed
// write for the wizard's progress. The state rides the config KV (no
// migration); the counts are the truth, so a stale dismissed flag cannot hide
// a setup that was later broken.

import (
	"net/http"

	"github.com/tubeof/proxyllm/internal/storage"
)

type onboardingCountsDTO struct {
	Providers    int `json:"providers"`
	LLMProviders int `json:"llm_providers"`
	WebTools     int `json:"web_tools"`
	Models       int `json:"models"`
	RoutingRules int `json:"routing_rules"`
	ClientKeys   int `json:"client_keys"`
}

type onboardingResponseDTO struct {
	NeedsSetup bool                    `json:"needs_setup"`
	State      storage.OnboardingState `json:"state"`
	Counts     onboardingCountsDTO     `json:"counts"`
}

// onboardingSteps is the closed vocabulary the wizard writes. Unknown step
// names are a 400, so a typo cannot silently mark the setup complete.
var onboardingSteps = map[string]bool{
	"provider":  true,
	"model":     true,
	"web_tools": true,
	"fallbacks": true,
	"routing":   true,
	"key":       true,
	"done":      true,
}

// getOnboarding serves GET /api/admin/onboarding — the wizard's status.
func (a *AdminAPI) getOnboarding(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	providers, err := a.store.ListProviders(ctx)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var counts onboardingCountsDTO
	counts.Providers = len(providers)
	for _, p := range providers {
		if desc, ok := providerTypeByID(p.Type); ok {
			switch desc.Kind {
			case KindLLM:
				counts.LLMProviders++
			case KindWebTool:
				counts.WebTools++
			}
		}
	}
	models, err := a.store.ListModels(ctx)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	counts.Models = len(models)
	rules, err := a.store.ListRoutingRules(ctx)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	counts.RoutingRules = len(rules)
	keys, err := a.store.ListClientKeys(ctx)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	counts.ClientKeys = len(keys)

	state, ok, err := a.store.GetOnboardingState(ctx)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if !ok {
		state = storage.OnboardingState{Version: storage.OnboardingStateVersion}
	}
	needsSetup := !state.Dismissed && state.CompletedAt == 0 &&
		(counts.LLMProviders == 0 || counts.Models == 0 || counts.ClientKeys == 0)
	writeJSON(w, http.StatusOK, onboardingResponseDTO{
		NeedsSetup: needsSetup,
		State:      state,
		Counts:     counts,
	})
}

// putOnboarding serves PUT /api/admin/onboarding — persist wizard progress.
func (a *AdminAPI) putOnboarding(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version     int      `json:"version"`
		Completed   []string `json:"completed"`
		Dismissed   bool     `json:"dismissed"`
		CompletedAt int64    `json:"completed_at"`
	}
	if err := decodeBody(w, r, &req); err != nil {
		return
	}
	seen := make(map[string]bool, len(req.Completed))
	completed := make([]string, 0, len(req.Completed))
	for _, step := range req.Completed {
		if !onboardingSteps[step] {
			writeError(w, http.StatusBadRequest, "unknown onboarding step: "+step)
			return
		}
		if !seen[step] {
			seen[step] = true
			completed = append(completed, step)
		}
	}
	version := req.Version
	if version == 0 {
		version = storage.OnboardingStateVersion
	}
	completedAt := req.CompletedAt
	if completedAt == 0 && seen["done"] {
		completedAt = a.now()
	}
	state := storage.OnboardingState{
		Version:     version,
		Completed:   completed,
		Dismissed:   req.Dismissed,
		CompletedAt: completedAt,
	}
	if err := a.store.PutOnboardingState(r.Context(), state); err != nil {
		writeStoreError(w, err)
		return
	}
	stored, _, err := a.store.GetOnboardingState(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": stored})
}
