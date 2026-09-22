package webui

// Model test endpoint (2026-09-22 Models page): POST /api/admin/models/{id}/test
// answers one minimal "Sag OK" round trip against the model row's provider
// so the operator can verify an account actually serves the model. The
// heavy lifting (registry lookup, the upstream call) is wired by
// server.go through AdminParams.TestModel — the handler only mounts,
// gates, and shapes the response.

import "net/http"

// TestModelResult is the test endpoint's wire shape. Ok carries the round
// trip's outcome; Error is a redacted plain-language cause when Ok is
// false (the operator needs a reason, never a raw upstream body).
type TestModelResult struct {
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	Reply     string `json:"reply,omitempty"`
	Error     string `json:"error,omitempty"`
}

// testModelHandler runs one model test. Unwired TestModel (no master key)
// answers the uniform 404 — the admin gate's degrade contract, identical
// to runModelSync.
func (a *AdminAPI) testModelHandler(w http.ResponseWriter, r *http.Request) {
	if a.testModel == nil {
		writeError(w, http.StatusNotFound, "model test is not wired")
		return
	}
	id := r.PathValue("id")
	result, err := a.testModel(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
