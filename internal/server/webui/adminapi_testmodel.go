package webui

// Model test endpoint (Models page): POST /api/admin/models/{id}/test
// answers one minimal "Sag OK" round trip against the model row's provider
// so the operator can verify an account actually serves the model. The
// heavy lifting (registry lookup, the upstream call) is wired by
// server.go through AdminParams.TestModel — the handler only mounts,
// gates, and shapes the response.

import "net/http"

// TestModelResult is the test endpoint's wire shape. Ok carries the round
// trip's outcome; Error is a redacted plain-language cause when Ok is
// false (the operator needs a reason, never a raw upstream body).
//
// Request and Detail exist so the popup can answer "why": what went out,
// and what the upstream said back — both redacted, both bounded.
type TestModelResult struct {
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	Reply     string `json:"reply,omitempty"`
	Error     string `json:"error,omitempty"`

	// Request is the wire body the test sent toward the provider, redacted
	// and bounded (provider.RequestPreviewer). Present on success too: the
	// operator compares what went out with what came back. Empty when the
	// provider cannot encode a preview or the encoding failed — a missing
	// diagnostic must never fail the test itself.
	Request string `json:"request,omitempty"`

	// Detail is the upstream's own account of a failure. Nil on success.
	Detail *TestFailureDetail `json:"detail,omitempty"`
}

// TestFailureDetail is the structured half of a failed round trip: the HTTP
// status and the decoded wire error object, all redacted.
type TestFailureDetail struct {
	// HTTPStatus is the upstream's status; 0 when no response arrived
	// (transport failure, timeout) — the honest "never reached it".
	HTTPStatus int `json:"http_status,omitempty"`
	// Type and Code are the decoded wire error.type / error.code ("" when
	// the body carried no error object).
	Type string `json:"type,omitempty"`
	Code string `json:"code,omitempty"`
	// Message is the upstream's message (or the transport error text),
	// redacted.
	Message string `json:"message,omitempty"`
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
