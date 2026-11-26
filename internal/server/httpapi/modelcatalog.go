package httpapi

// GET /v1/model_catalog: the Codex-native model catalog (ADR-023). Codex
// reads it through model_providers.<id>.model_catalog_url with the
// [features] api_key_model_discovery switch; the shape is a top-level
// "models" array (never the OpenAI "data" list) whose entry fields come from
// codex-rs's openai_models::Model. supported_reasoning_levels is always an
// array: codex 0.153 rejects a catalog whose entries omit it.

import (
	"encoding/json"
	"net/http"

	"github.com/tubeof/proxyllm/internal/storage"
)

// handleModelCatalog serves the Codex-shaped catalog.
func (a *api) handleModelCatalog(w http.ResponseWriter, r *http.Request) {
	models, err := a.store.ListModels(r.Context())
	if err != nil {
		models = nil
	}
	entries := make([]catalogModel, 0, len(models))
	for i, m := range models {
		display := m.Name
		if display == "" {
			display = m.ID
		}
		entry := catalogModel{
			Slug:                     m.ID,
			DisplayName:              display,
			Description:              "Model " + m.ID + " on provider " + m.ProviderID,
			SupportedInAPI:           true,
			Visibility:               "list",
			Priority:                 i,
			SupportedReasoningLevels: []any{},
			InputModalities:          []string{"text"},
			UseResponsesLite:         false,
		}
		if window := contextWindow(m); window > 0 {
			entry.ContextWindow = window
			entry.MaxContextWindow = window
		}
		if hasCapability(m, "vision") {
			entry.InputModalities = append(entry.InputModalities, "image")
		}
		entries = append(entries, entry)
	}
	writeJSON(w, openAIErrorWriter(), http.StatusOK, struct {
		Models []catalogModel `json:"models"`
	}{Models: entries})
}

// catalogModel is one Codex catalog entry. Optional fields carry omitempty;
// supported_reasoning_levels never does (see the file header).
type catalogModel struct {
	Slug                     string   `json:"slug"`
	DisplayName              string   `json:"display_name"`
	Description              string   `json:"description,omitempty"`
	SupportedInAPI           bool     `json:"supported_in_api"`
	Visibility               string   `json:"visibility"`
	Priority                 int      `json:"priority"`
	ContextWindow            int      `json:"context_window,omitempty"`
	MaxContextWindow         int      `json:"max_context_window,omitempty"`
	SupportedReasoningLevels []any    `json:"supported_reasoning_levels"`
	InputModalities          []string `json:"input_modalities"`
	UseResponsesLite         bool     `json:"use_responses_lite"`
}

// contextWindow reads compat.context_window from a model row.
func contextWindow(m storage.Model) int {
	var compat struct {
		ContextWindow int `json:"context_window"`
	}
	if len(m.Compat) > 0 {
		_ = json.Unmarshal(m.Compat, &compat)
	}
	return compat.ContextWindow
}

// hasCapability reads one boolean from the capabilities blob.
func hasCapability(m storage.Model, name string) bool {
	var caps map[string]bool
	if len(m.Capabilities) > 0 {
		_ = json.Unmarshal(m.Capabilities, &caps)
	}
	return caps[name]
}
