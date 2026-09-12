package httpapi

// OpenAI-surface RED tests: POST /v1/chat/completions (O2) and GET
// /v1/models (O9). Derived from plan §5.1/§5.3, wire facts O10/O11/O3/O4,
// golden fixtures testdata/golden/openai/*.json, and the §6.4 compat
// policy. The upstream mock speaks the OpenAI chat wire — same-surface
// relay must be fixture-exact for the decodable fields.
//
// RED state: package httpapi does not exist yet (build failure proves RED
// for every test in this file).

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/proxyllm/proxyllm/internal/routing"
	"github.com/proxyllm/proxyllm/internal/testutil/mockprovider"
)

// mockJSON builds one upstream script answering a JSON body.
func mockJSON(t *testing.T, status int, body string) *mockprovider.Script {
	t.Helper()
	return mockprovider.NewScript().JSONResponse(status, body, nil)
}

// mockSSE builds one upstream script answering an SSE stream.
func mockSSE(t *testing.T, raw string) *mockprovider.Script {
	t.Helper()
	return mockprovider.NewScript().SSEFrames(raw)
}

// mockFail builds one upstream script answering a non-2xx JSON body.
func mockFail(t *testing.T, status int, body string) *mockprovider.Script {
	t.Helper()
	return mockprovider.NewScript().Fail(status, body, "")
}

// --- POST /v1/chat/completions (non-streaming) ---

// 1. Fixture round trip: a golden chat request routed to a mock upstream
// that answers with the golden chat response must surface the upstream
// answer as a valid chat.completion (O2): the proxy decodes the client
// body, routes, executes, and re-encodes for the client.
func TestChatCompletions_FixtureRoundTrip(t *testing.T) {
	e := newEnv(t)
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))
	status, body := e.post(t, "/v1/chat/completions", string(golden(t, "openai/chat-request.json")))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}
	var resp struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Index        int    `json:"index"`
			Message      any    `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage any `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("response not JSON: %v (%s)", err, body)
	}
	if resp.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", resp.Object)
	}
	if resp.ID != "chatcmpl-abc123" {
		t.Errorf("id = %q, want upstream id chatcmpl-abc123 relayed verbatim", resp.ID)
	}
	if resp.Model != "gpt-5" {
		t.Errorf("model = %q, want gpt-5", resp.Model)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("choices = %+v, want one choice with finish_reason tool_calls (O3/O12)", resp.Choices)
	}
	if resp.Usage == nil {
		t.Error("usage missing — chat.completion must carry usage")
	}
}

// 2. The mock upstream must receive the request in the OpenAI chat wire
// shape with the routed model name: the proxy translated the client IR
// back to chat-wire before dispatch (same-surface relay).
func TestChatCompletions_UpstreamSeesChatWire(t *testing.T) {
	e := newEnv(t)
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))
	if _, body := e.post(t, "/v1/chat/completions", string(golden(t, "openai/chat-request.json"))); body == "" {
		t.Fatal("empty client response")
	}

	reqs := e.mock.Requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(reqs))
	}
	var upstream struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(reqs[0].Body), &upstream); err != nil {
		t.Fatalf("upstream body not chat-wire JSON: %v (%s)", err, reqs[0].Body)
	}
	if upstream.Model != "gpt-5" {
		t.Errorf("upstream model = %q, want the requested gpt-5 (chain entry model)", upstream.Model)
	}
	if upstream.Stream {
		t.Error("upstream saw stream=true for a non-streaming client request")
	}
}

// 3. Error relay per O10/§5.3: an upstream 429 exhausts the retry budget
// and the single-provider chain; the client sees a protocol-conform
// OpenAI error body (error.type/code/message) with status 429 — the
// terminal chain error, never a bare Go error string.
func TestChatCompletions_Upstream429Error(t *testing.T) {
	e := newEnv(t)
	// 429 → ActionRetrySame (default MaxRetriesPerProvider 2 → three
	// attempts) → chain exhausted → terminal error surfaces.
	for i := 0; i < 3; i++ {
		e.mock.Enqueue(mockFail(t, 429, string(golden(t, "openai/error.json"))))
	}
	status, body := e.post(t, "/v1/chat/completions", string(golden(t, "openai/chat-request.json")))
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body %s)", status, body)
	}
	var wireErr struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &wireErr); err != nil {
		t.Fatalf("error body not O10-shaped: %v (%s)", err, body)
	}
	if wireErr.Error.Message == "" || wireErr.Error.Type == "" {
		t.Errorf("error body missing type/message (O10): %s", body)
	}
	// Upstream credentials never echoed (§5.3): the mock upstream was
	// called with Authorization: Bearer mock-key — the error body must not
	// contain it.
	if strings.Contains(body, "mock-key") {
		t.Error("error body leaks upstream credential")
	}
}

// 4. No-route: a request for a model with no rule → 4xx client error per
// §5.3 with an O10-shaped body.
func TestChatCompletions_NoRoute(t *testing.T) {
	e := newEnv(t)
	req := `{"model":"unknown-model","messages":[{"role":"user","content":"hi"}]}`
	status, body := e.post(t, "/v1/chat/completions", req)
	if status < 400 || status >= 500 {
		t.Fatalf("no-route status = %d (%s), want 4xx", status, body)
	}
	var wireErr struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &wireErr); err != nil {
		t.Fatalf("no-route body not O10-shaped: %v (%s)", err, body)
	}
	if wireErr.Error.Message == "" || wireErr.Error.Type == "" {
		t.Errorf("no-route error body missing fields: %s", body)
	}
}

// 5. Request log row (§4.3 observability): one successful request must
// persist exactly one request_logs row with client_key_id, provider_id,
// model, status 200, upstream-relayed usage, and a route_trace object.
func TestChatCompletions_RequestLogRow(t *testing.T) {
	e := newEnv(t)
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))
	if _, body := e.post(t, "/v1/chat/completions", string(golden(t, "openai/chat-request.json"))); body == "" {
		t.Fatal("empty response")
	}

	logs := e.requestLogs(t)
	if len(logs) != 1 {
		t.Fatalf("request_logs rows = %d, want 1", len(logs))
	}
	l := logs[0]
	if l.ClientKeyID != e.keyID {
		t.Errorf("client_key_id = %q, want the authenticated key id", l.ClientKeyID)
	}
	if l.ProviderID != "mock-main" {
		t.Errorf("provider_id = %q, want mock-main", l.ProviderID)
	}
	if l.Model != "gpt-5" {
		t.Errorf("model = %q, want gpt-5", l.Model)
	}
	if l.Status != 200 {
		t.Errorf("status = %d, want 200", l.Status)
	}
	var usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	}
	if err := json.Unmarshal(l.Usage, &usage); err != nil {
		t.Fatalf("usage JSON invalid: %v (%s)", err, l.Usage)
	}
	if usage.InputTokens != 1250 || usage.OutputTokens != 77 {
		t.Errorf("usage = %d/%d, want upstream usage 1250/77 relayed", usage.InputTokens, usage.OutputTokens)
	}
	var trace map[string]any
	if err := json.Unmarshal(l.RouteTrace, &trace); err != nil {
		t.Fatalf("route_trace not JSON object: %v (%s)", err, l.RouteTrace)
	}
}

// 6. Streaming relay per C2 (OpenAI wire: data-only SSE per O4): the
// proxy relays upstream SSE frames as-they-arrive, terminating with
// [DONE], and the client-visible deltas match the upstream fixture.
func TestChatCompletions_StreamRelay(t *testing.T) {
	e := newEnv(t)
	e.mock.Enqueue(mockSSE(t, string(golden(t, "openai/chat-stream.sse"))))
	req := strings.Replace(string(golden(t, "openai/chat-request.json")), `"stream": false`, `"stream": true`, 1)
	status, body := e.postStream(t, "/v1/chat/completions", req)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}
	frames := sseDataFrames(body)
	if len(frames) < 3 {
		t.Fatalf("SSE frames = %d, want >= 3 (deltas + usage + [DONE])", len(frames))
	}
	if frames[len(frames)-1] != "[DONE]" {
		t.Errorf("last frame = %q, want [DONE] (O4)", frames[len(frames)-1])
	}
	if !strings.Contains(body, `"Hello"`) || !strings.Contains(body, `" world"`) {
		t.Errorf("relayed stream missing upstream text deltas: %s", body)
	}
}

// 7. Streaming request log: a completed stream persists its request log
// row with the streamed terminal usage (1250/128 from the fixture's
// terminal usage chunk).
func TestChatCompletions_StreamRequestLog(t *testing.T) {
	e := newEnv(t)
	e.mock.Enqueue(mockSSE(t, string(golden(t, "openai/chat-stream.sse"))))
	req := strings.Replace(string(golden(t, "openai/chat-request.json")), `"stream": false`, `"stream": true`, 1)
	if _, body := e.postStream(t, "/v1/chat/completions", req); body == "" {
		t.Fatal("empty stream")
	}
	logs := e.requestLogs(t)
	if len(logs) != 1 {
		t.Fatalf("request_logs rows = %d, want 1", len(logs))
	}
	var usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	}
	if err := json.Unmarshal(logs[0].Usage, &usage); err != nil {
		t.Fatalf("usage JSON invalid: %v (%s)", err, logs[0].Usage)
	}
	if usage.InputTokens != 1250 || usage.OutputTokens != 128 {
		t.Errorf("streamed usage = %d/%d, want 1250/128 from the terminal chunk", usage.InputTokens, usage.OutputTokens)
	}
}

// --- GET /v1/models (O9) ---

// 8. GET /v1/models lists every model row from the store (O9 shape:
// {"object":"list","data":[...]} with object:"model" entries), ordered by
// id (storage contract).
func TestModels_List(t *testing.T) {
	e := newEnv(t)
	e.seedModel(t, "gpt-5", nil)
	e.seedModel(t, "gpt-5-mini", nil)
	status, body := e.get(t, "/v1/models")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}
	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID     string `json:"id"`
			Object string `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("models body not JSON: %v (%s)", err, body)
	}
	if resp.Object != "list" {
		t.Errorf("object = %q, want list", resp.Object)
	}
	ids := map[string]bool{}
	for _, m := range resp.Data {
		ids[m.ID] = true
		if m.Object != "model" {
			t.Errorf("model object = %q, want model", m.Object)
		}
	}
	if !ids["gpt-5"] || !ids["gpt-5-mini"] {
		t.Errorf("model ids = %v, want gpt-5 and gpt-5-mini", ids)
	}
}

// 9. GET /v1/models on an empty store → honest empty list (O9 shape).
func TestModels_EmptyList(t *testing.T) {
	e := newEnv(t)
	status, body := e.get(t, "/v1/models")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}
	var resp struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("empty models body not O9-shaped: %v (%s)", err, body)
	}
	if resp.Object != "list" || resp.Data == nil {
		t.Errorf("empty models body = %s, want object:list data:[]", body)
	}
}

// --- compat policies (§6.4 — first ApplyCompat caller) ---

// 10. thinking block (§6.4): a model configured with
// thinking_handling=block rejects a thinking request with a
// protocol-conform 400 (O10) and a request_logs row recording the
// rejection (never silent).
func TestChatCompletions_ThinkingBlock(t *testing.T) {
	e := newEnv(t)
	e.seedModel(t, "gpt-5", json.RawMessage(`{"thinking_handling":"block"}`))
	req := `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`
	status, body := e.post(t, "/v1/chat/completions", req)
	if status != http.StatusBadRequest {
		t.Fatalf("thinking-block status = %d, want 400 (body %s)", status, body)
	}
	var wireErr struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &wireErr); err != nil {
		t.Fatalf("thinking-block body not JSON: %v (%s)", err, body)
	}
	logs := e.requestLogs(t)
	if len(logs) == 0 {
		t.Fatal("thinking-block rejection left no request log row")
	}
}

// 11. thinking strip (§6.4 default): history thinking blocks + the
// thinking param are stripped before upstream dispatch; the request
// succeeds and the strip is traced.
func TestChatCompletions_ThinkingStrip(t *testing.T) {
	e := newEnv(t)
	e.seedModel(t, "gpt-5", json.RawMessage(`{"thinking_handling":"strip"}`))
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))
	req := `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`
	status, body := e.post(t, "/v1/chat/completions", req)
	if status != http.StatusOK {
		t.Fatalf("strip status = %d, want 200 (body %s)", status, body)
	}
	reqs := e.mock.Requests()
	if len(reqs) == 0 {
		t.Fatal("upstream saw no request")
	}
	if strings.Contains(string(reqs[0].Body), "reasoning_effort") {
		t.Errorf("upstream body carries reasoning_effort despite strip: %s", reqs[0].Body)
	}
	logs := e.requestLogs(t)
	if len(logs) == 0 {
		t.Fatal("strip request left no log row")
	}
}

// 12. max_tokens_clamp (§6.4): a model configured with max_tokens_clamp
// clamps the client max tokens before upstream dispatch.
func TestChatCompletions_MaxTokensClamp(t *testing.T) {
	e := newEnv(t)
	e.seedModel(t, "gpt-5", json.RawMessage(`{"max_tokens_clamp":1024}`))
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))
	req := `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":4096}`
	status, body := e.post(t, "/v1/chat/completions", req)
	if status != http.StatusOK {
		t.Fatalf("clamp status = %d, want 200 (body %s)", status, body)
	}
	reqs := e.mock.Requests()
	if len(reqs) == 0 {
		t.Fatal("upstream saw no request")
	}
	var upstream struct {
		MaxCompletionTokens int `json:"max_completion_tokens"`
	}
	if err := json.Unmarshal([]byte(reqs[0].Body), &upstream); err != nil {
		t.Fatalf("upstream body invalid: %v", err)
	}
	if upstream.MaxCompletionTokens != 1024 {
		t.Errorf("upstream max_completion_tokens = %d, want clamped to 1024", upstream.MaxCompletionTokens)
	}
}

// --- failover on the surface (§5.3 + Analysis E) ---

// 13. Failover: a two-provider chain where the first fails with 500 and
// the second succeeds → the client sees 200 and both providers attempted.
func TestChatCompletions_Failover(t *testing.T) {
	e := newEnvWithRules(t, routing.Rule{
		MatchModel: "gpt-5",
		Chain:      []string{"mock-main", "mock-main"},
		Priority:   100,
		Enabled:    true,
	})
	e.mock.Enqueue(mockFail(t, 500, `{"error":{"message":"boom","type":"server_error","code":null}}`))
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))
	status, body := e.post(t, "/v1/chat/completions", string(golden(t, "openai/chat-request.json")))
	if status != http.StatusOK {
		t.Fatalf("failover status = %d, want 200 (body %s)", status, body)
	}
	reqs := e.mock.Requests()
	if len(reqs) != 2 {
		t.Errorf("upstream attempts = %d, want 2 (primary failed, fallback served)", len(reqs))
	}
}
