package httpapi

// OpenAI Responses-surface RED tests: POST /v1/responses (O5–O8,
// user-confirmed v1 scope in plan §5.1). The proxy decodes the
// Responses-wire client request into IR, routes through the chat-wire
// mock upstream (custom provider class), and re-encodes the upstream IR
// into Responses wire for the client — so the enqueued scripts speak
// openai/chat-response.json / openai/chat-stream.sse while the client
// sends testdata/golden/openai/responses-request.json and receives
// Responses-shaped output.
//
// Streaming relays the Responses semantic events (O7): response.created,
// response.output_text.delta, response.function_call_arguments.delta,
// response.completed, carrying the terminal upstream usage.
//
// RED state: package httpapi does not exist yet (build failure proves RED
// for every test in this file).

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/testutil/mockprovider"
)

// sseEvent is one named SSE frame parsed off a Responses stream body.
type sseEvent struct {
	name string // event: line value
	data string // data: payload
}

// parseSSEEvents splits an SSE body into (event name, data) pairs. Frames
// without an event: line carry "" (O4 data-only frames are tolerated on
// this surface too).
func parseSSEEvents(body string) []sseEvent {
	var out []sseEvent
	for _, frame := range strings.Split(body, "\n\n") {
		var ev sseEvent
		for _, line := range strings.Split(frame, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			}
		}
		if ev.name != "" || ev.data != "" {
			out = append(out, ev)
		}
	}
	return out
}

// --- POST /v1/responses (non-streaming) ---

// 1. Fixture round trip: a golden Responses request routed to a mock
// chat-wire upstream answering with the golden chat response must
// surface the answer as a completed Responses object (O5): typed output
// items, relayed text, and upstream usage mapped into Responses usage.
func TestResponses_FixtureRoundTrip(t *testing.T) {
	e := newEnv(t)
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))
	status, body := e.post(t, "/v1/responses", string(golden(t, "openai/responses-request.json")))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}
	var resp struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Status string `json:"status"`
		Model  string `json:"model"`
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("response not JSON: %v (%s)", err, body)
	}
	if resp.Object != "response" {
		t.Errorf("object = %q, want response (O5)", resp.Object)
	}
	if resp.Status != "completed" {
		t.Errorf("status = %q, want completed (O5)", resp.Status)
	}
	if resp.Model != "gpt-5" {
		t.Errorf("model = %q, want gpt-5", resp.Model)
	}
	if resp.ID == "" {
		t.Error("id missing — Responses objects are addressable")
	}
	var textOut string
	haveMessage := false
	for _, item := range resp.Output {
		if item.Type != "message" {
			continue
		}
		haveMessage = true
		for _, part := range item.Content {
			if part.Type == "output_text" {
				textOut += part.Text
			}
		}
	}
	if !haveMessage {
		t.Errorf("output items = %+v, want at least one type:message item (O5)", resp.Output)
	}
	if !strings.Contains(textOut, "I'll check the weather for you.") {
		t.Errorf("output_text = %q, want the upstream assistant text relayed", textOut)
	}
	if resp.Usage.InputTokens != 1250 || resp.Usage.OutputTokens != 77 {
		t.Errorf("usage = %d/%d, want upstream usage 1250/77 relayed", resp.Usage.InputTokens, resp.Usage.OutputTokens)
	}
}

// 2. The mock upstream must receive the translated request in the OpenAI
// chat wire shape: model, non-streaming, messages carrying the Responses
// input items (incl. the get_weather function-call history, O6/O3).
func TestResponses_UpstreamSeesChatWire(t *testing.T) {
	e := newEnv(t)
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))
	if _, body := e.post(t, "/v1/responses", string(golden(t, "openai/responses-request.json"))); body == "" {
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
		t.Errorf("upstream model = %q, want gpt-5", upstream.Model)
	}
	if upstream.Stream {
		t.Error("upstream saw stream=true for a non-streaming client request")
	}
	if len(upstream.Messages) < 1 {
		t.Fatalf("upstream messages = %d, want the translated input items", len(upstream.Messages))
	}
	if !strings.Contains(reqs[0].Body, "get_weather") {
		t.Errorf("upstream body missing the get_weather tool history (O6): %s", reqs[0].Body)
	}
}

// 3. Streaming relay (O7): the client sees the Responses semantic events —
// response.created, response.output_text.delta with the upstream text
// deltas relayed, terminal response.completed carrying the streamed
// terminal usage (1250/128).
func TestResponses_StreamRelay(t *testing.T) {
	e := newEnv(t)
	e.mock.Enqueue(mockSSE(t, string(golden(t, "openai/chat-stream.sse"))))
	req := strings.Replace(string(golden(t, "openai/responses-request.json")), `"stream": false`, `"stream": true`, 1)
	status, body := e.postStream(t, "/v1/responses", req)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}

	created, delta, completed := false, false, -1
	var deltas []string
	var terminalUsage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	}
	for i, ev := range parseSSEEvents(body) {
		switch ev.name {
		case "response.created":
			created = true
		case "response.output_text.delta":
			delta = true
			var d struct {
				Delta string `json:"delta"`
			}
			if err := json.Unmarshal([]byte(ev.data), &d); err != nil {
				t.Fatalf("output_text.delta data not JSON: %v (%s)", err, ev.data)
			}
			deltas = append(deltas, d.Delta)
		case "response.completed":
			completed = i
			var full struct {
				Response struct {
					Usage struct {
						InputTokens  int `json:"input_tokens"`
						OutputTokens int `json:"output_tokens"`
					} `json:"usage"`
				} `json:"response"`
			}
			if err := json.Unmarshal([]byte(ev.data), &full); err != nil {
				t.Fatalf("response.completed data not JSON: %v (%s)", err, ev.data)
			}
			terminalUsage = full.Response.Usage
		}
	}
	if !created {
		t.Error("stream missing response.created (O7)")
	}
	if !delta {
		t.Error("stream missing response.output_text.delta (O7)")
	}
	joined := strings.Join(deltas, "")
	if !strings.Contains(joined, "Hello") || !strings.Contains(joined, " world") {
		t.Errorf("relayed deltas = %q, want the upstream Hello/world text deltas", joined)
	}
	if completed != len(parseSSEEvents(body))-1 {
		t.Errorf("response.completed at index %d of %d, want it terminal (O7)", completed, len(parseSSEEvents(body)))
	}
	if terminalUsage.InputTokens != 1250 || terminalUsage.OutputTokens != 128 {
		t.Errorf("terminal usage = %d/%d, want 1250/128 from the streamed final chunk", terminalUsage.InputTokens, terminalUsage.OutputTokens)
	}
}

// 4. Tool-call stream (O7): upstream chat tool_calls deltas translate to
// Responses function_call items with function-call argument deltas.
func TestResponses_ToolCallStream(t *testing.T) {
	e := newEnv(t)
	e.mock.Enqueue(mockSSE(t, string(golden(t, "openai/chat-stream.sse"))))
	req := strings.Replace(string(golden(t, "openai/responses-request.json")), `"stream": false`, `"stream": true`, 1)
	status, body := e.postStream(t, "/v1/responses", req)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}
	var argDeltas []string
	for _, ev := range parseSSEEvents(body) {
		if ev.name != "response.function_call_arguments.delta" {
			continue
		}
		var d struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal([]byte(ev.data), &d); err != nil {
			t.Fatalf("function_call_arguments.delta data not JSON: %v (%s)", err, ev.data)
		}
		argDeltas = append(argDeltas, d.Delta)
	}
	if len(argDeltas) == 0 {
		t.Fatalf("no response.function_call_arguments.delta events — tool call not translated to a Responses function_call (O7): %s", body)
	}
	joined := strings.Join(argDeltas, "")
	if !strings.Contains(joined, "location") {
		t.Errorf("assembled arguments = %q, want the get_weather location argument", joined)
	}
}

// 5. Mid-stream upstream failure (Analysis E failover discipline): once
// streaming has begun, an upstream connection drop terminates with the
// protocol-conform Responses error event (O7: FLAT {type,code,message}
// shape) — never a mid-stream provider swap, never a bare Go error string.
func TestResponses_StreamMidStreamError(t *testing.T) {
	e := newEnv(t)
	firstDelta := `data: {"id":"x","object":"chat.completion.chunk","model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n"
	e.mock.Enqueue(mockprovider.NewScript().SSEFrames(firstDelta).Abort())

	req := strings.Replace(string(golden(t, "openai/responses-request.json")), `"stream": false`, `"stream": true`, 1)
	status, body := e.postStream(t, "/v1/responses", req)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SSE already started, body %s)", status, body)
	}
	var errEvent *sseEvent
	for i, ev := range parseSSEEvents(body) {
		if ev.name != "error" {
			continue
		}
		if errEvent != nil {
			t.Errorf("multiple error events in %s — error must be terminal (O7)", body)
		}
		errEvent = &parseSSEEvents(body)[i]
	}
	if errEvent == nil {
		t.Fatalf("mid-stream failure produced no error event (O7): %s", body)
	}
	var wireErr struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(errEvent.data), &wireErr); err != nil {
		t.Fatalf("error event data not JSON: %v (%s)", err, errEvent.data)
	}
	if wireErr.Type != "error" {
		t.Errorf("error event type = %q, want \"error\" (O7 flat shape)", wireErr.Type)
	}
	if wireErr.Message == "" {
		t.Errorf("error event missing message: %s", errEvent.data)
	}
	// Terminal discipline: nothing after the error event.
	events := parseSSEEvents(body)
	if len(events) > 0 && events[len(events)-1].name != "error" {
		t.Errorf("events continue after the terminal error event (B2 discipline): %s", body)
	}
}

// 4a wiring (Responses surface): the mid-stream failure must reach the
// shared HealthTracker via ReportStreamOutcome — dispatch reported
// success, the relayed Error event corrects it to a failure.
func TestResponses_StreamMidStreamErrorReportsHealthFailure(t *testing.T) {
	e := newEnv(t)
	firstDelta := `data: {"id":"x","object":"chat.completion.chunk","model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n"
	e.mock.Enqueue(mockprovider.NewScript().SSEFrames(firstDelta).Abort())

	req := strings.Replace(string(golden(t, "openai/responses-request.json")), `"stream": false`, `"stream": true`, 1)
	status, body := e.postStream(t, "/v1/responses", req)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SSE already started, body %s)", status, body)
	}
	if !strings.Contains(body, "\"type\":\"error\"") {
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

// 6. No-route (§5.3): a request for a model with no rule → 4xx with the
// OpenAI error shape (O10).
func TestResponses_NoRoute(t *testing.T) {
	e := newEnv(t)
	status, body := e.post(t, "/v1/responses", `{"model":"unknown-model","input":"hi"}`)
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
