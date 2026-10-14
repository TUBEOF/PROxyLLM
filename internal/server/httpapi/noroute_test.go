package httpapi

// Spec-derived: an unroutable model answers the reference's model-not-found
// envelope — HTTP 404, error type not_found_error, and a message naming the
// model as "model: <id>" (the reference's own wording for an unknown model).
// Claude Code classifies a response as model-not-found only when the status
// is 404 AND the body's error type is not_found_error AND the message
// contains "model:" — and only that classification triggers its model
// fallback. A prose message reads as an opaque 404, so the client cannot
// recover and every auxiliary request (the WebFetch apply call, the
// auto-mode classifier) dies. Both surfaces answer their own envelope with
// the same message; the streaming surface answers it as a pre-stream HTTP
// error, exactly like the non-streaming one.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestNoRoute_ModelNotFoundContract(t *testing.T) {
	e := newEnv(t) // routes gpt-5 → mock-main only

	for _, tc := range []struct {
		name string
		path string
		body string
	}{
		{
			"anthropic non-stream",
			"/v1/messages",
			`{"model":"claude-sonnet-4-6","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			"anthropic stream",
			"/v1/messages",
			`{"model":"claude-sonnet-4-6","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			"openai",
			"/v1/chat/completions",
			`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`,
		},
	} {
		status, body := e.post(t, tc.path, tc.body)
		if status != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404 (body %s)", tc.name, status, body)
			continue
		}
		var payload struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Errorf("%s: body is not JSON: %v (%s)", tc.name, err, body)
			continue
		}
		if payload.Error.Type != "not_found_error" {
			t.Errorf("%s: error.type = %q, want not_found_error", tc.name, payload.Error.Type)
		}
		if !strings.Contains(payload.Error.Message, "model: claude-sonnet-4-6") {
			t.Errorf("%s: error.message = %q, want it to name the model as \"model: <id>\"", tc.name, payload.Error.Message)
		}
	}
}
