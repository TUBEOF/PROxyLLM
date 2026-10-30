package httpapi

// Request-decode failures are the client's, never a 500. docs/api/errors.md
// lists body decode failure as 400, but /v1/messages, /v1/chat/completions and
// /v1/responses routed it through the pipeline mapper and answered 500. The
// production report was a Claude Code replay whose text block carried a
// citations ARRAY — a legal shape the codec also had to accept.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestMessages_TextCitationsArrayDecodesAndForwards(t *testing.T) {
	e := newClaudeEnv(t)
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))

	body := string(golden(t, "anthropic/message-request-citations.json"))
	status, resp := e.postAnthropic(t, "/v1/messages", body)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, resp)
	}
	reqs := e.mock.Requests()
	if len(reqs) != 1 || !strings.Contains(reqs[0].Body, "The answer is 42.") {
		t.Fatalf("upstream body = %+v, want the cited text forwarded", reqs)
	}
}

func TestMessages_MalformedRequestIs400Not500(t *testing.T) {
	e := newClaudeEnv(t)
	status, resp := e.postAnthropic(t, "/v1/messages",
		`{"model":"claude-sonnet-5","max_tokens":"not-a-number"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", status, resp)
	}
	var wire struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(resp), &wire); err != nil {
		t.Fatalf("body not the Anthropic error envelope: %v (%s)", err, resp)
	}
	if wire.Type != "error" || wire.Error.Type != "invalid_request_error" || wire.Error.Message == "" {
		t.Errorf("envelope = %+v, want invalid_request_error", wire)
	}
	logs := e.requestLogs(t)
	if len(logs) != 1 || logs[0].Status != http.StatusBadRequest {
		t.Errorf("request logs = %+v, want one 400 row", logs)
	}
}

func TestChatAndResponses_MalformedRequestIs400Not500(t *testing.T) {
	for _, tc := range []struct {
		path string
		body string
	}{
		{"/v1/chat/completions", `{"model":"gpt-5","messages":"not-an-array"}`},
		{"/v1/responses", `{"model":"gpt-5","input":"x","max_output_tokens":"nope"}`},
	} {
		e := newEnv(t)
		status, resp := e.post(t, tc.path, tc.body)
		if status != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400 (body %s)", tc.path, status, resp)
		}
	}
}

// TestEmptyRequestIs400NotForwarded: a request with no system prompt and no
// messages has nothing to translate, and every upstream chat wire requires at
// least one message — Ollama Cloud answers the empty body with
// `[] is too short - 'messages' (ref: …)` (observed live 2026-10-30). The
// refusal must be local: a 400 for the client, no dispatch to the provider, and
// a log row that names the actual problem instead of an upstream ref.
func TestEmptyRequestIs400NotForwarded(t *testing.T) {
	for _, tc := range []struct {
		path string
		body string
	}{
		{"/v1/chat/completions", `{"model":"gpt-5","messages":[]}`},
		{"/v1/responses", `{"model":"gpt-5","input":[]}`},
		{"/v1/messages", `{"model":"claude-sonnet-5","max_tokens":16,"messages":[]}`},
		{"/api/chat", `{"model":"llama-4","messages":[]}`},
	} {
		e := newEnv(t)
		status, resp := e.post(t, tc.path, tc.body)
		if status != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400 (body %s)", tc.path, status, resp)
		}
		if reqs := e.mock.Requests(); len(reqs) != 0 {
			t.Errorf("%s dispatched %d upstream request(s), want none", tc.path, len(reqs))
		}
	}
}
