package httpapi

// GET /v1/models (plan §5.1, wire facts O9 + C1/C3): one path, two shapes.
// Without the anthropic-version header the OpenAI list shape answers
// ({"object":"list","data":[...]}); with it, the Anthropic discovery shape
// ({"data":[...],"first_id":..,"has_more":false,"last_id":..}) — the
// gateway model discovery Claude Code opts into (C1). ListModels orders by
// id ASC, which fixes first_id/last_id.

import (
	"encoding/json"
	"net/http"

	"github.com/tubeof/proxyllm/internal/storage"
)

// anthropicVersionHeader switches the model-list shape (C3).
const anthropicVersionHeader = "anthropic-version"

// handleModels serves GET /v1/models in the surface-appropriate shape.
func (a *api) handleModels(w http.ResponseWriter, r *http.Request) {
	models, err := a.store.ListModels(r.Context())
	if err != nil {
		// Honest empty beat a broken list: the store owns the truth.
		models = nil
	}

	if r.Header.Get(anthropicVersionHeader) != "" {
		a.writeAnthropicModels(w, models)
		return
	}
	a.writeOpenAIModels(w, models)
}

// writeOpenAIModels emits the O9 shape: {"object":"list","data":[...]}.
// data is never null (the empty store answers an honest empty array).
func (a *api) writeOpenAIModels(w http.ResponseWriter, models []storage.Model) {
	type modelEntry struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Owned  string `json:"owned_by"`
	}
	entries := make([]modelEntry, 0, len(models))
	for _, m := range models {
		entries = append(entries, modelEntry{ID: m.ID, Object: "model", Owned: m.ProviderID})
	}
	writeJSON(w, openAIErrorWriter(), http.StatusOK, struct {
		Object string       `json:"object"`
		Data   []modelEntry `json:"data"`
	}{Object: "list", Data: entries})
}

// writeAnthropicModels emits the C1/C3 discovery shape: data entries with
// id/type/display_name plus the first_id/has_more/last_id envelope.
func (a *api) writeAnthropicModels(w http.ResponseWriter, models []storage.Model) {
	type modelEntry struct {
		ID          string `json:"id"`
		Type        string `json:"type"`
		DisplayName string `json:"display_name"`
	}
	entries := make([]modelEntry, 0, len(models))
	firstID, lastID := "", ""
	for _, m := range models {
		display := m.Name
		if display == "" {
			display = m.ID
		}
		entries = append(entries, modelEntry{ID: m.ID, Type: "model", DisplayName: display})
		if firstID == "" {
			firstID = m.ID
		}
		lastID = m.ID
	}
	writeJSON(w, anthropicErrorWriter(), http.StatusOK, struct {
		Data    []modelEntry `json:"data"`
		FirstID string       `json:"first_id"`
		HasMore bool         `json:"has_more"`
		LastID  string       `json:"last_id"`
	}{Data: entries, FirstID: firstID, HasMore: false, LastID: lastID})
}

// writeJSON renders one JSON response body. ew is the surface-appropriate
// error emitter: a marshal failure must answer in the shape the requested
// surface speaks, not a hardcoded OpenAI body (review m4).
func writeJSON(w http.ResponseWriter, ew errorWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		ew.write(w, http.StatusInternalServerError, errTypeUpstream, "encoding failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
