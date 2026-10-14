package httpapi

// Anthropic-surface RED tests: POST /v1/messages (incl. ?beta=true), POST
// /v1/messages/count_tokens (heuristic marking), and the Anthropic-shaped
// GET /v1/models. Derived from the wire contracts, golden fixtures
// testdata/golden/anthropic/* plus testdata/golden/openai/* (the mock
// upstream speaks OpenAI chat wire — these tests define the cross-surface
// translation contract: Anthropic client in, OpenAI upstream, Anthropic
// response out).

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/testutil/mockprovider"
)

// newClaudeEnv assembles the cross-surface environment: one rule routing
// the Anthropic fixture model to the OpenAI-wire mock upstream.
func newClaudeEnv(t *testing.T) *env {
	t.Helper()
	e := newEnvWithRules(t, routing.Rule{
		MatchModel: "claude-sonnet-5",
		Chain:      []string{"mock-main"},
		Priority:   100,
		Enabled:    true,
	})
	e.seedModel(t, "claude-sonnet-5", nil)
	return e
}

// claudeStreamReq returns the golden Anthropic request with stream:true
// added (an SSE request). Fails loudly if the golden anchor string
// ever moves, so the test can never silently degrade to non-streaming.
func claudeStreamReq(t *testing.T) string {
	t.Helper()
	const anchor = `"stop_sequences": ["END"]`
	raw := string(golden(t, "anthropic/message-request.json"))
	if !strings.Contains(raw, anchor) {
		t.Fatalf("golden anthropic/message-request.json no longer contains anchor %q", anchor)
	}
	req := strings.Replace(raw, anchor, `"stop_sequences": ["END"], "stream": true`, 1)
	if !strings.Contains(req, `"stream": true`) {
		t.Fatal("stream flag not injected into golden request")
	}
	return req
}

// postAnthropic sends one authenticated POST with x-api-key — the
// header family Anthropic clients actually send.
func (e *env) postAnthropic(t *testing.T, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", e.key)
	return e.do(t, req)
}

// getWith sends one authenticated GET carrying an extra header (e.g.
// anthropic-version).
func (e *env) getWith(t *testing.T, path, header, value string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.key)
	if header != "" {
		req.Header.Set(header, value)
	}
	return e.do(t, req)
}

// sseEventNames extracts the event-field values of an SSE body in order.
func sseEventNames(body string) []string {
	var out []string
	for _, frame := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			if after, ok := strings.CutPrefix(line, "event: "); ok {
				out = append(out, after)
			}
		}
	}
	return out
}

// sseEventData returns the data payload of the first frame with the given
// event name ("" when absent).
func sseEventData(body, event string) string {
	for _, frame := range strings.Split(body, "\n\n") {
		var name, data string
		for _, line := range strings.Split(frame, "\n") {
			if after, ok := strings.CutPrefix(line, "event: "); ok {
				name = after
			}
			if after, ok := strings.CutPrefix(line, "data: "); ok {
				data = after
			}
		}
		if name == event && data != "" {
			return data
		}
	}
	return ""
}

// --- POST /v1/messages (non-streaming, cross-surface) ---

// 1. Fixture round trip (cross-surface): an Anthropic Messages
// request routed to an OpenAI-wire upstream must come back as a
// protocol-conform Anthropic message — stop_reason tool_use translated
// from upstream finish_reason tool_calls, text + tool_use blocks
// assembled, usage with explicit cache fields.
func TestMessages_FixtureRoundTrip(t *testing.T) {
	e := newClaudeEnv(t)
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))

	status, body := e.postAnthropic(t, "/v1/messages", string(golden(t, "anthropic/message-request.json")))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}
	var resp struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Role       string `json:"role"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("response not JSON: %v (%s)", err, body)
	}
	if resp.Type != "message" {
		t.Errorf("type = %q, want the message envelope", resp.Type)
	}
	if resp.Role != "assistant" {
		t.Errorf("role = %q, want assistant", resp.Role)
	}
	if resp.Model != "claude-sonnet-5" {
		t.Errorf("model = %q, want claude-sonnet-5", resp.Model)
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use (translated from upstream finish_reason tool_calls)", resp.StopReason)
	}
	var sawText, sawToolUse bool
	for _, blk := range resp.Content {
		switch blk.Type {
		case "text":
			if blk.Text == "I'll check the weather for you." {
				sawText = true
			}
		case "tool_use":
			if blk.Name == "get_weather" && blk.ID == "call_abc123" {
				sawToolUse = true
			}
		}
	}
	if !sawText {
		t.Errorf("content missing the upstream text block: %s", body)
	}
	if !sawToolUse {
		t.Errorf("content missing tool_use block {get_weather, call_abc123} relayed from upstream tool_calls: %s", body)
	}

	// Usage: upstream prompt/completion tokens map to input/output, and
	// the Anthropic cache usage fields must be present explicitly (zeros
	// where unknown — never silently dropped).
	var usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	}
	if err := json.Unmarshal(jsonField(t, body, "usage"), &usage); err != nil {
		t.Fatalf("usage not JSON: %v (%s)", err, body)
	}
	if usage.InputTokens != 1250 || usage.OutputTokens != 77 {
		t.Errorf("usage = %d/%d, want upstream 1250/77 mapped to input/output", usage.InputTokens, usage.OutputTokens)
	}
	var usageFields map[string]json.RawMessage
	if err := json.Unmarshal(jsonField(t, body, "usage"), &usageFields); err != nil {
		t.Fatal(err)
	}
	if _, ok := usageFields["cache_creation_input_tokens"]; !ok {
		t.Errorf("usage missing cache_creation_input_tokens (explicit field): %s", body)
	}
	if _, ok := usageFields["cache_read_input_tokens"]; !ok {
		t.Errorf("usage missing cache_read_input_tokens (explicit field): %s", body)
	}
}

// 2. Cross-surface request translation: the OpenAI-wire upstream must
// receive a chat-completions body — model carried, tools translated to
// function definitions, no stream flag.
func TestMessages_UpstreamSeesChatWire(t *testing.T) {
	e := newClaudeEnv(t)
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))
	if _, body := e.postAnthropic(t, "/v1/messages", string(golden(t, "anthropic/message-request.json"))); body == "" {
		t.Fatal("empty client response")
	}

	reqs := e.mock.Requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(reqs))
	}
	var upstream struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
		Tools  []struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(reqs[0].Body), &upstream); err != nil {
		t.Fatalf("upstream body not chat-wire JSON: %v (%s)", err, reqs[0].Body)
	}
	if upstream.Model != "claude-sonnet-5" {
		t.Errorf("upstream model = %q, want the requested claude-sonnet-5", upstream.Model)
	}
	if upstream.Stream {
		t.Error("upstream saw stream=true for a non-streaming client request")
	}
	if len(upstream.Tools) == 0 || upstream.Tools[0].Function.Name != "get_weather" {
		t.Errorf("upstream tools = %+v, want get_weather translated to a function definition (translation matrix)", upstream.Tools)
	}
}

// 3. Streaming relay: upstream chat-wire SSE deltas surface as
// named Anthropic events in the canonical order, ending with
// message_stop; tool_calls deltas become a sequential tool_use block
// with input_json_delta fragments.
func TestMessages_StreamRelay(t *testing.T) {
	e := newClaudeEnv(t)
	e.mock.Enqueue(mockSSE(t, string(golden(t, "openai/chat-stream.sse"))))

	status, body := e.postStream(t, "/v1/messages", claudeStreamReq(t))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}
	events := sseEventNames(body)
	has := func(name string) bool {
		for _, ev := range events {
			if ev == name {
				return true
			}
		}
		return false
	}
	if !has("message_start") || !has("content_block_delta") {
		t.Errorf("SSE events = %v, want message_start and content_block_delta", events)
	}
	if len(events) == 0 || events[len(events)-1] != "message_stop" {
		t.Errorf("last event = %v, want message_stop terminating the stream", events)
	}
	if !strings.Contains(body, "Hello") || !strings.Contains(body, " world") {
		t.Errorf("relayed stream missing upstream text deltas: %s", body)
	}
	if !strings.Contains(body, "tool_use") || !strings.Contains(body, "input_json_delta") {
		t.Errorf("tool_calls deltas not translated to tool_use block + input_json_delta: %s", body)
	}
}

// 4. Mid-stream upstream failure (failover discipline):
// once streaming has begun, an upstream connection drop terminates with
// a protocol-conform Anthropic error event — never a mid-stream provider
// swap, never a bare Go error string.
func TestMessages_StreamMidStreamError(t *testing.T) {
	e := newClaudeEnv(t)
	firstDelta := `data: {"id":"x","object":"chat.completion.chunk","model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n"
	e.mock.Enqueue(mockprovider.NewScript().SSEFrames(firstDelta).Abort())

	status, body := e.postStream(t, "/v1/messages", claudeStreamReq(t))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SSE already started, body %s)", status, body)
	}
	if !strings.Contains(body, "event: error") {
		t.Fatalf("mid-stream failure produced no error event: %s", body)
	}
	data := sseEventData(body, "error")
	if data == "" {
		t.Fatalf("error event carries no data payload: %s", body)
	}
	var ev struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		t.Fatalf("error event data not JSON: %v (%s)", err, data)
	}
	if ev.Error.Type == "" || ev.Error.Message == "" {
		t.Errorf("error event missing error.type/error.message: %s", data)
	}
}

// 4a wiring: a mid-stream upstream failure must reach the shared
// HealthTracker via ReportStreamOutcome — dispatch reported success, the
// relayed Error event corrects it to a failure. One request leaves the
// breaker closed (threshold 3); two more direct failures on top open it,
// proving the mid-stream failure counted.
func TestMessages_StreamMidStreamErrorReportsHealthFailure(t *testing.T) {
	e := newClaudeEnv(t)
	firstDelta := `data: {"id":"x","object":"chat.completion.chunk","model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n"
	e.mock.Enqueue(mockprovider.NewScript().SSEFrames(firstDelta).Abort())

	status, body := e.postStream(t, "/v1/messages", claudeStreamReq(t))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SSE already started, body %s)", status, body)
	}
	if !strings.Contains(body, "event: error") {
		t.Fatalf("mid-stream failure produced no error event: %s", body)
	}

	// The mid-stream failure was reported: 1 failure so far, threshold 3.
	if e.health.Cooldown("mock-main") {
		t.Fatal("breaker open after one mid-stream failure (threshold 3)")
	}
	e.health.ReportFailure("mock-main")
	e.health.ReportFailure("mock-main")
	if !e.health.Cooldown("mock-main") {
		t.Error("mid-stream failure must count toward the circuit breaker")
	}
}

// 5. count_tokens heuristic: with an OpenAI-shaped upstream the
// native Anthropic token counting is impossible, so the heuristic
// estimate (chars/4) must answer — marked as heuristic, never
// presented as an exact count.
func TestMessages_CountTokensHeuristic(t *testing.T) {
	e := newClaudeEnv(t)
	status, body := e.postAnthropic(t, "/v1/messages/count_tokens",
		string(golden(t, "anthropic/count-tokens-request.json")))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}
	var resp struct {
		InputTokens int    `json:"input_tokens"`
		Estimation  string `json:"estimation"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("count_tokens body not JSON: %v (%s)", err, body)
	}
	if resp.InputTokens <= 0 {
		t.Errorf("input_tokens = %d, want a positive heuristic estimate", resp.InputTokens)
	}
	if resp.Estimation != "heuristic" {
		t.Errorf("estimation = %q, want the \"heuristic\" marking — never silently exact", resp.Estimation)
	}
}

// --- GET /v1/models shape switching ---

// 6. Model discovery serves BOTH shapes on the same path: with the
// anthropic-version header the Anthropic list shape (data with
// id/type/display_name + first_id/has_more/last_id), without it the
// OpenAI list shape (object:"list").
func TestMessages_ModelsDiscoveryBothShapes(t *testing.T) {
	e := newEnv(t)
	e.seedModel(t, "gpt-5", nil)
	e.seedModel(t, "gpt-5-mini", nil)

	// Anthropic shape (fixture: testdata/golden/anthropic/models.json).
	status, body := e.getWith(t, "/v1/models", "anthropic-version", "2023-06-01")
	if status != http.StatusOK {
		t.Fatalf("anthropic models status = %d, want 200 (body %s)", status, body)
	}
	var anth struct {
		Data []struct {
			ID          string `json:"id"`
			Type        string `json:"type"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
		FirstID string `json:"first_id"`
		HasMore bool   `json:"has_more"`
		LastID  string `json:"last_id"`
	}
	if err := json.Unmarshal([]byte(body), &anth); err != nil {
		t.Fatalf("anthropic models body not JSON: %v (%s)", err, body)
	}
	ids := map[string]bool{}
	for _, m := range anth.Data {
		ids[m.ID] = true
		if m.Type != "model" {
			t.Errorf("anthropic model type = %q, want model", m.Type)
		}
		if m.DisplayName == "" {
			t.Errorf("model %s missing display_name", m.ID)
		}
	}
	if !ids["gpt-5"] || !ids["gpt-5-mini"] {
		t.Errorf("anthropic models ids = %v, want gpt-5 and gpt-5-mini", ids)
	}
	if anth.FirstID != "gpt-5" || anth.LastID != "gpt-5-mini" || anth.HasMore {
		t.Errorf("anthropic envelope = %s/%v/%s, want gpt-5/false/gpt-5-mini", anth.FirstID, anth.HasMore, anth.LastID)
	}

	// OpenAI shape without the header.
	status, body = e.get(t, "/v1/models")
	if status != http.StatusOK {
		t.Fatalf("openai models status = %d, want 200 (body %s)", status, body)
	}
	var openai struct {
		Object string `json:"object"`
	}
	if err := json.Unmarshal([]byte(body), &openai); err != nil {
		t.Fatalf("openai models body not JSON: %v (%s)", err, body)
	}
	if openai.Object != "list" {
		t.Errorf("openai models object = %q, want list", openai.Object)
	}
}

// --- error shape on the Anthropic surface ---

// 7. No-route: a request for a model with no rule answers with the
// Anthropic error envelope {type:"error", error:{type,message}}.
func TestMessages_NoRoute(t *testing.T) {
	e := newEnv(t)
	req := `{"model":"unknown-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	status, body := e.postAnthropic(t, "/v1/messages", req)
	if status < 400 || status >= 500 {
		t.Fatalf("no-route status = %d (%s), want 4xx", status, body)
	}
	var wireErr struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &wireErr); err != nil {
		t.Fatalf("no-route body not Anthropic-shaped: %v (%s)", err, body)
	}
	if wireErr.Type != "error" {
		t.Errorf("top-level type = %q, want error", wireErr.Type)
	}
	if wireErr.Error.Type == "" || wireErr.Error.Message == "" {
		t.Errorf("error object missing type/message: %s", body)
	}
}

// 8. ?beta=true path match: Claude Code addresses /v1/messages with
// a beta query string — the same handler must serve it.
func TestMessages_BetaTruePathMatch(t *testing.T) {
	e := newClaudeEnv(t)
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))
	status, body := e.postAnthropic(t, "/v1/messages?beta=true", string(golden(t, "anthropic/message-request.json")))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}
	var resp struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("response not JSON: %v (%s)", err, body)
	}
	if resp.Type != "message" {
		t.Errorf("type = %q, want message", resp.Type)
	}
}

// 9. Auth-disabled path through httpapi: with the middleware's ENV
// opt-out applied, requests
// with NO key at all pass on both surfaces — Bearer/x-api-key absent —
// while the same requests against the default (auth-on) env are rejected.
func TestMessages_AuthDisabledNoKeyPath(t *testing.T) {
	e := newEnvAuthDisabled(t)
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))

	// Keyless Anthropic-surface POST must pass.
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/v1/messages",
		strings.NewReader(string(golden(t, "anthropic/message-request.json"))))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	status, body := e.do(t, req)
	if status != http.StatusOK {
		t.Fatalf("keyless status = %d, want 200 with auth disabled (body %s)", status, body)
	}
	var resp struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("response not JSON: %v (%s)", err, body)
	}
	if resp.Type != "message" {
		t.Errorf("type = %q, want message", resp.Type)
	}

	// Keyless OpenAI-surface POST must pass too.
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))
	req2, err := http.NewRequest(http.MethodPost, e.srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req2.Header.Set("Content-Type", "application/json")
	status2, body2 := e.do(t, req2)
	if status2 != http.StatusOK {
		t.Fatalf("keyless OpenAI-surface status = %d, want 200 (body %s)", status2, body2)
	}

	// Control: the auth-on default still rejects keyless requests.
	eOn := newClaudeEnv(t)
	eOn.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))
	req3, err := http.NewRequest(http.MethodPost, eOn.srv.URL+"/v1/messages",
		strings.NewReader(string(golden(t, "anthropic/message-request.json"))))
	if err != nil {
		t.Fatal(err)
	}
	req3.Header.Set("Content-Type", "application/json")
	status3, _ := eOn.do(t, req3)
	if status3 != http.StatusUnauthorized {
		t.Errorf("auth-on keyless status = %d, want 401 (control)", status3)
	}
}

// Health-cooldown terminal mapping: a chain whose every provider is in
// circuit-breaker cooldown answers with the RETRYABLE overload signal,
// not the non-retryable 404 "model may not exist" that makes Claude
// Code demand a /model switch. Spec: docs/api-reference.md error model —
// "upstream overload maps to 529 semantics (overloaded_error)" on the
// Anthropic surface, 503 on the OpenAI surfaces; the golden
// anthropic/stream-error fixture correlates overloaded_error with 529.
func TestMessages_HealthCooldownAnswersOverloadedNot404(t *testing.T) {
	e := newClaudeEnv(t)

	// Open the breaker: 3 consecutive failures is the configured
	// threshold (HealthTracker defaults, rules_test.go pins the circuit
	// behavior — this test pins only the terminal mapping).
	e.health.ReportFailure("mock-main")
	e.health.ReportFailure("mock-main")
	e.health.ReportFailure("mock-main")
	if !e.health.Cooldown("mock-main") {
		t.Fatal("precondition: breaker must be open after 3 failures")
	}

	// Anthropic surface: 529 overloaded_error (error envelope).
	status, body := e.postAnthropic(t, "/v1/messages", string(golden(t, "anthropic/message-request.json")))
	if status != 529 {
		t.Fatalf("status = %d, want 529 (body %s)", status, body)
	}
	var a13 struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &a13); err != nil {
		t.Fatalf("body not the Anthropic error JSON: %v (%s)", err, body)
	}
	if a13.Type != "error" || a13.Error.Type != "overloaded_error" || a13.Error.Message == "" {
		t.Errorf("envelope = %+v, want type=error, error.type=overloaded_error, message set", a13)
	}

	// The request-log row carries the status actually written — the
	// WebUI must show the retryable failure, not a misleading 404.
	logs := e.requestLogs(t)
	if len(logs) != 1 || logs[0].Status != 529 {
		t.Errorf("request log = %+v, want exactly one row with status 529", logs)
	}
}

// Same terminal condition on the OpenAI surfaces: 503 (the documented
// OpenAI-family status for service unavailability), never 404.
func TestChat_HealthCooldownAnswers503Not404(t *testing.T) {
	e := newEnv(t)
	e.health.ReportFailure("mock-main")
	e.health.ReportFailure("mock-main")
	e.health.ReportFailure("mock-main")
	if !e.health.Cooldown("mock-main") {
		t.Fatal("precondition: breaker must be open after 3 failures")
	}

	status, body := e.post(t, "/v1/chat/completions", string(golden(t, "openai/chat-request.json")))
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", status, body)
	}
	var o10 struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &o10); err != nil {
		t.Fatalf("body not the OpenAI error JSON: %v (%s)", err, body)
	}
	if o10.Error.Type == "" || o10.Error.Message == "" {
		t.Errorf("error = %+v, want type and message set", o10.Error)
	}

	logs := e.requestLogs(t)
	if len(logs) != 1 || logs[0].Status != http.StatusServiceUnavailable {
		t.Errorf("request log = %+v, want exactly one row with status 503", logs)
	}
}

// Cross-surface regression for the Ollama "invalid message content
// type: <nil>" 400: an Anthropic-surface conversation history carrying
// a tool_use-only assistant turn (the standard agent-loop shape Claude
// Code sends back) must reach the upstream OpenAI-wire body with
// "content": "" — never "content": null, which Ollama's
// OpenAI-compatibility endpoint rejects with HTTP 400 (ollama
// openai/openai.go, "invalid message content type: %T" on nil;
// mnfst/manifest#2721 reproduces it on ollama-cloud).
func TestMessages_ToolOnlyHistoryEncodesEmptyStringContent(t *testing.T) {
	e := newClaudeEnv(t)
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))

	// Anthropic history: user asks, assistant answers with a tool_use
	// block ONLY (no text block) — exactly the shape Claude Code replays
	// in every agent loop after a tool call.
	body := `{
		"model": "claude-sonnet-5",
		"max_tokens": 1024,
		"messages": [
			{"role": "user", "content": "Weather in SF?"},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"location": "SF"}}
			]}
		],
		"tools": [{"name": "get_weather", "description": "Get weather", "input_schema": {"type": "object"}}]
	}`
	if _, respBody := e.postAnthropic(t, "/v1/messages", body); respBody == "" {
		t.Fatal("empty client response")
	}

	reqs := e.mock.Requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(reqs))
	}
	up := reqs[0].Body
	if strings.Contains(up, `"content":null`) {
		t.Errorf("upstream body carries \"content\":null — Ollama's compat endpoint answers 400 invalid message content type: <nil>: %s", up)
	}
	if !strings.Contains(up, `"content":""`) {
		t.Errorf("upstream body must carry empty-string content for the tool-only assistant turn: %s", up)
	}
}

// Second cross-surface regression for the Ollama "invalid message content
// type: <nil>" 400: Claude Code appends a role:"system" message to
// messages (SessionStart-hook output, injected reminders). The Anthropic
// codec decodes it into ir.RoleSystem; the OpenAI chat encoder's role
// switch must carry a RoleSystem case: without one the zero-value wireContent
// marshals as "content":null and Ollama's OpenAI-compatibility endpoint
// answers 400 (reproduced with a real Claude Code client against the
// proxy). The system message must reach the upstream wire as a system-role
// message
// carrying its text.
func TestMessages_SystemRoleHistoryEncodesSystemMessageContent(t *testing.T) {
	e := newClaudeEnv(t)
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))

	body := `{
		"model": "claude-sonnet-5",
		"max_tokens": 1024,
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "system", "content": [{"type": "text", "text": "SessionStart hook output"}]}
		]
	}`
	if _, respBody := e.postAnthropic(t, "/v1/messages", body); respBody == "" {
		t.Fatal("empty client response")
	}

	reqs := e.mock.Requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(reqs))
	}
	up := reqs[0].Body
	if strings.Contains(up, `"content":null`) {
		t.Errorf(`upstream body carries "content":null — Ollama's compat endpoint answers 400 invalid message content type: <nil>: %s`, up)
	}
	if !strings.Contains(up, `"role":"system"`) {
		t.Errorf(`upstream body must keep the system-role message: %s`, up)
	}
	if !strings.Contains(up, "SessionStart hook output") {
		t.Errorf(`upstream body must carry the system message text: %s`, up)
	}
}

// Context-marker strip must extend to the compat lookup: a suffixed
// request routes to the base model, and the compat policy
// lives on the BASE row — the marker must not silently bypass the
// operator's thinking handling / clamps (a lookup keyed on the typed name
// finds no row, so the zero-value defaults would run instead of the
// operator's policy). Pinned on the visible wire
// effect: the base row's max_tokens_clamp must cap the suffixed
// request's max_completion_tokens, and its thinking_handling=block must
// refuse the request with 400 (the marker's presence must not restore
// default-strip behavior for neither).
func TestMessages_SuffixedModelAppliesBaseCompatPolicy(t *testing.T) {
	e := newEnvWithRules(t, routing.Rule{
		MatchModel: "glm-5.3-flash", Chain: []string{"mock-main"}, Priority: 100, Enabled: true,
	})
	e.seedModel(t, "glm-5.3-flash", []byte(`{"thinking_handling":"block","max_tokens_clamp":8}`))

	// Clamp: the 1024-token suffixed request must reach the wire capped
	// to the base row's clamp of 8.
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))
	body := `{
		"model": "glm-5.3-flash[1M]",
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": "hello"}]
	}`
	if code, _ := e.postAnthropic(t, "/v1/messages", body); code != http.StatusOK {
		t.Fatalf("suffixed request status = %d, want 200", code)
	}
	reqs := e.mock.Requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(reqs))
	}
	if reqs[0].Body == "" {
		t.Fatal("upstream body is empty")
	}
	var sent struct {
		MaxCompletionTokens int `json:"max_completion_tokens"`
	}
	if err := json.Unmarshal([]byte(reqs[0].Body), &sent); err != nil {
		t.Fatalf("upstream body not JSON: %v (%s)", err, reqs[0].Body)
	}
	if sent.MaxCompletionTokens != 8 {
		t.Errorf("base row's max_tokens_clamp must cap the marker-bearing request: max_completion_tokens = %d, want 8", sent.MaxCompletionTokens)
	}

	// Block: the operator's thinking_handling=block must refuse the
	// marker-bearing thinking request exactly like the unsuffixed one.
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))
	thinking := `{
		"model": "glm-5.3-flash[1M]",
		"max_tokens": 1024,
		"thinking": {"type": "enabled", "budget_tokens": 2048},
		"messages": [{"role": "user", "content": "hello"}]
	}`
	code, _ := e.postAnthropic(t, "/v1/messages", thinking)
	if code != http.StatusBadRequest {
		t.Errorf("thinking_handling=block must refuse the marker-bearing thinking request, got status %d — the marker must not bypass the base policy", code)
	}
}
