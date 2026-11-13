package e2e

// Conformance matrix, second slice: the Ollama native surface (client and
// upstream), tool-call round trips, and the fault profiles — an upstream that
// rate-limits and one that truncates mid-stream. These cells are the cases a
// green happy-path suite never sees.

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	openaiclient "github.com/openai/openai-go/v3"

	"github.com/tubeof/proxyllm/internal/storage"
)

// ollamaPost sends one raw Ollama-native chat request to the proxy (the client
// surface), authenticated with the test key, and returns the response.
func (e *matrixEnv) ollamaPost(t *testing.T, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/api/chat", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build ollama request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("ollama request: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// 6. Ollama native client → Ollama native upstream, NDJSON streaming.
func TestMatrix_OllamaClient_OllamaUpstream(t *testing.T) {
	up := upstream{ID: "ollama-native-up", Type: "ollama-native"}
	e := newMatrixEnv(t, up)
	e.ms.Upsert(t, expectation("OLLAMA", up.ID, "/api/chat",
		chatCompletion("Native Ollama answer", true)))

	resp := e.ollamaPost(t, `{"model":"`+up.ID+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var text strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var line struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &line) == nil {
			text.WriteString(line.Message.Content)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read native stream: %v", err)
	}
	if !strings.Contains(text.String(), "Native Ollama answer") {
		t.Fatalf("client text = %q, want the simulated native answer", text.String())
	}

	body := e.upstreamBody(t, "/api/chat")
	if body["model"] != up.ID {
		t.Errorf("upstream model = %v, want %s", body["model"], up.ID)
	}
}

// 7. Cross-protocol: OpenAI Chat client → Ollama native upstream.
func TestMatrix_ChatClient_OllamaNativeUpstream(t *testing.T) {
	ctx := context.Background()
	up := upstream{ID: "ollama-native-cross", Type: "ollama-native"}
	e := newMatrixEnv(t, up)
	e.ms.Upsert(t, expectation("OLLAMA", up.ID, "/api/chat",
		chatCompletion("Native cross answer", true)))

	stream := e.openai.Chat.Completions.NewStreaming(ctx, openaiclient.ChatCompletionNewParams{
		Model:    up.ID,
		Messages: []openaiclient.ChatCompletionMessageParamUnion{openaiclient.UserMessage("hi")},
	})
	defer func() { _ = stream.Close() }()
	var text strings.Builder
	for stream.Next() {
		if choices := stream.Current().Choices; len(choices) > 0 {
			text.WriteString(choices[0].Delta.Content)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("chat stream: %v", err)
	}
	if !strings.Contains(text.String(), "Native cross answer") {
		t.Fatalf("client text = %q, want the native upstream's text", text.String())
	}
	if body := e.upstreamBody(t, "/api/chat"); body["model"] != up.ID {
		t.Errorf("upstream model = %v, want %s", body["model"], up.ID)
	}
}

// 8. Tool call: OpenAI Chat client → OpenAI Responses upstream. The client
// must see a function tool call with the arguments, and the upstream request
// must carry the declared tool.
func TestMatrix_ChatClient_ResponsesUpstream_ToolCall(t *testing.T) {
	ctx := context.Background()
	up := upstream{ID: "oa-responses-tools", Type: "openai-responses"}
	e := newMatrixEnv(t, up)
	e.ms.Upsert(t, map[string]any{
		"httpRequest": map[string]any{"method": "POST", "path": "/v1/responses"},
		"httpLlmResponse": map[string]any{
			"provider": "OPENAI_RESPONSES",
			"model":    up.ID,
			"completion": map[string]any{
				"stopReason": "tool_calls",
				"toolCalls": []map[string]any{{
					"id":        "call_weather_1",
					"name":      "get_weather",
					"arguments": `{"city":"Paris"}`,
				}},
			},
		},
	})

	cc, err := e.openai.Chat.Completions.New(ctx, openaiclient.ChatCompletionNewParams{
		Model:    up.ID,
		Messages: []openaiclient.ChatCompletionMessageParamUnion{openaiclient.UserMessage("weather in Paris?")},
		Tools: []openaiclient.ChatCompletionToolUnionParam{{
			OfFunction: &openaiclient.ChatCompletionFunctionToolParam{
				Function: openaiclient.FunctionDefinitionParam{
					Name:       "get_weather",
					Parameters: map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("chat completion: %v", err)
	}
	if len(cc.Choices) == 0 || len(cc.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("client tool calls = %+v, want one function call", cc.Choices)
	}
	call := cc.Choices[0].Message.ToolCalls[0]
	if call.Function.Name != "get_weather" || !strings.Contains(call.Function.Arguments, "Paris") {
		t.Errorf("tool call = %+v, want get_weather with the Paris arguments", call)
	}

	body := e.upstreamBody(t, "/v1/responses")
	if tools, _ := body["tools"].([]any); len(tools) == 0 {
		t.Errorf("upstream request carries no tools: %v", body)
	}
}

// 9. Fault: the upstream rate-limits (429 with Retry-After). The client must
// receive a typed upstream error, never a fabricated answer.
func TestMatrix_ChatClient_UpstreamRateLimited(t *testing.T) {
	ctx := context.Background()
	up := upstream{ID: "rate-limited-up", Type: "ollama-cloud"}
	e := newMatrixEnv(t, up)
	e.ms.Upsert(t, map[string]any{
		"httpRequest": map[string]any{"method": "POST", "path": "/v1/chat/completions"},
		"httpLlmResponse": map[string]any{
			"provider": "OPENAI",
			"model":    up.ID,
			"completion": map[string]any{
				"text": "never delivered",
			},
			"chaos": map[string]any{
				"errorStatus":      429,
				"errorProbability": 1.0,
				"retryAfter":       "1",
			},
		},
	})

	_, err := e.openai.Chat.Completions.New(ctx, openaiclient.ChatCompletionNewParams{
		Model:    up.ID,
		Messages: []openaiclient.ChatCompletionMessageParamUnion{openaiclient.UserMessage("hi")},
	})
	if err == nil {
		t.Fatal("client received an answer from a rate-limited upstream")
	}
	var apiErr *openaiclient.Error
	if !asOpenAIError(err, &apiErr) {
		t.Fatalf("error = %v, want the SDK's API error", err)
	}
	if apiErr.StatusCode < 400 {
		t.Errorf("status = %d, want an upstream error status", apiErr.StatusCode)
	}
}

// 10. Fault: the upstream truncates the stream halfway. The client must see a
// terminal error (or a partial answer plus an error), never a silent success.
func TestMatrix_ChatClient_UpstreamTruncatedStream(t *testing.T) {
	ctx := context.Background()
	up := upstream{ID: "truncating-up", Type: "ollama-cloud"}
	e := newMatrixEnv(t, up)
	e.ms.Upsert(t, map[string]any{
		"httpRequest": map[string]any{"method": "POST", "path": "/v1/chat/completions"},
		"httpLlmResponse": map[string]any{
			"provider": "OPENAI",
			"model":    up.ID,
			"completion": map[string]any{
				"text":      "This answer will be cut in the middle by the chaos profile",
				"streaming": true,
			},
			"chaos": map[string]any{
				"truncateMode":       "MID_STREAM",
				"truncateAtFraction": 0.5,
				"malformedSse":       false,
			},
		},
	})

	stream := e.openai.Chat.Completions.NewStreaming(ctx, openaiclient.ChatCompletionNewParams{
		Model:    up.ID,
		Messages: []openaiclient.ChatCompletionMessageParamUnion{openaiclient.UserMessage("hi")},
	})
	defer func() { _ = stream.Close() }()
	for stream.Next() {
		_ = stream.Current()
	}
	if stream.Err() == nil {
		t.Fatal("a truncated upstream stream completed without an error")
	}
}

// 11. Fault: the upstream answers 529 (the Anthropic overload status the
// OpenAI-compatible wire also uses). It is a server error, so the proxy fails
// over without a same-provider retry; a single-provider chain surfaces the 529
// and never fabricates an answer.
func TestMatrix_ChatClient_UpstreamOverloaded529(t *testing.T) {
	ctx := context.Background()
	up := upstream{ID: "overloaded-up", Type: "ollama-cloud"}
	e := newMatrixEnv(t, up)
	e.ms.Upsert(t, map[string]any{
		"httpRequest": map[string]any{"method": "POST", "path": "/v1/chat/completions"},
		"httpLlmResponse": map[string]any{
			"provider": "OPENAI",
			"model":    up.ID,
			"completion": map[string]any{
				"text": "never delivered",
			},
			"chaos": map[string]any{
				"errorStatus":      529,
				"errorProbability": 1.0,
				"retryAfter":       "1",
			},
		},
	})

	_, err := e.openai.Chat.Completions.New(ctx, openaiclient.ChatCompletionNewParams{
		Model:    up.ID,
		Messages: []openaiclient.ChatCompletionMessageParamUnion{openaiclient.UserMessage("hi")},
	})
	if err == nil {
		t.Fatal("client received an answer from an overloaded upstream")
	}
	var apiErr *openaiclient.Error
	if !asOpenAIError(err, &apiErr) {
		t.Fatalf("error = %v, want the SDK's API error", err)
	}
	if apiErr.StatusCode != 529 {
		t.Errorf("status = %d, want the upstream's 529", apiErr.StatusCode)
	}
	if n := countUpstreamRequests(t, e, "/v1/chat/completions"); n != 1 {
		t.Errorf("upstream requests = %d, want exactly 1 (server errors fail over, never retry)", n)
	}
}

// 12. Fault: the upstream stream carries a malformed SSE frame. MockServer
// appends its malformed chunk after the terminal sentinel for sentinel-
// terminated wires, which those decoders never read — so the fault rides a
// GEMINI upstream (no sentinel) served as a raw SSE body with the broken frame
// before the terminal. The client must see the partial answer and a terminal
// error, never a clean success.
func TestMatrix_ChatClient_UpstreamMalformedStream(t *testing.T) {
	ctx := context.Background()
	up := upstream{ID: "gemini-malformed-up", Type: "gemini"}
	// pairWire keys off the manifest id "gemini-native"; this upstream has its
	// own id, so build the same path convention directly.
	path := "/v1beta/models/" + up.ID + ":streamGenerateContent"

	run := func(t *testing.T, body string) (string, error) {
		t.Helper()
		e := newMatrixEnv(t, up)
		e.ms.Upsert(t, map[string]any{
			"httpRequest": map[string]any{"method": "POST", "path": path},
			"httpResponse": map[string]any{
				"statusCode": 200,
				"headers": []map[string]any{{
					"name": "Content-Type", "values": []string{"text/event-stream"},
				}},
				"body": body,
			},
		})
		stream := e.openai.Chat.Completions.NewStreaming(ctx, openaiclient.ChatCompletionNewParams{
			Model:    up.ID,
			Messages: []openaiclient.ChatCompletionMessageParamUnion{openaiclient.UserMessage("hi")},
		})
		defer func() { _ = stream.Close() }()
		var text strings.Builder
		for stream.Next() {
			if choices := stream.Current().Choices; len(choices) > 0 {
				text.WriteString(choices[0].Delta.Content)
			}
		}
		if n := countUpstreamRequests(t, e, path); n != 1 {
			t.Errorf("upstream requests = %d, want exactly 1", n)
		}
		return text.String(), stream.Err()
	}

	t.Run("fault", func(t *testing.T) {
		text, err := run(t, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial-\"}]}}]}\n\n"+
			"data: {not json\n\n")
		if !strings.Contains(text, "partial-") {
			t.Fatalf("client text = %q, want the partial delta before the malformed frame", text)
		}
		if err == nil {
			t.Fatal("a malformed upstream SSE stream completed without an error")
		}
	})

	t.Run("control", func(t *testing.T) {
		text, err := run(t, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"complete\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}\n\n")
		if err != nil {
			t.Fatalf("clean Gemini stream errored: %v", err)
		}
		if !strings.Contains(text, "complete") {
			t.Fatalf("client text = %q, want the clean answer", text)
		}
	})
}

// 13. Fault: MockServer's stateful request-count quota. The first request is
// served; the next one over the limit gets 429 with Retry-After: 60. That
// reset exceeds the proxy's backoff ceiling, so the same provider is not
// retried — the upstream sees exactly two requests — and the client ends with
// a typed 429 error, never a fabricated answer.
func TestMatrix_ChatClient_UpstreamQuotaWindow(t *testing.T) {
	ctx := context.Background()
	up := upstream{ID: "quota-window-up", Type: "ollama-cloud"}
	e := newMatrixEnv(t, up)
	e.ms.Upsert(t, map[string]any{
		"httpRequest": map[string]any{"method": "POST", "path": "/v1/chat/completions"},
		"httpLlmResponse": map[string]any{
			"provider": "OPENAI",
			"model":    up.ID,
			"completion": map[string]any{
				"text": "served before the quota window closed",
			},
			"chaos": map[string]any{
				"quotaName":         "matrix-quota-window",
				"quotaLimit":        1,
				"quotaWindowMillis": 60000,
			},
		},
	})

	first, err := e.openai.Chat.Completions.New(ctx, openaiclient.ChatCompletionNewParams{
		Model:    up.ID,
		Messages: []openaiclient.ChatCompletionMessageParamUnion{openaiclient.UserMessage("hi")},
	})
	if err != nil {
		t.Fatalf("first request inside the quota window: %v", err)
	}
	if len(first.Choices) == 0 || !strings.Contains(first.Choices[0].Message.Content, "served before") {
		t.Fatalf("first answer = %+v, want the served completion", first.Choices)
	}

	_, err = e.openai.Chat.Completions.New(ctx, openaiclient.ChatCompletionNewParams{
		Model:    up.ID,
		Messages: []openaiclient.ChatCompletionMessageParamUnion{openaiclient.UserMessage("again")},
	})
	if err == nil {
		t.Fatal("client received an answer after the quota window closed")
	}
	var apiErr *openaiclient.Error
	if !asOpenAIError(err, &apiErr) {
		t.Fatalf("error = %v, want the SDK's API error", err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 from the quota window", apiErr.StatusCode)
	}
	if n := countUpstreamRequests(t, e, "/v1/chat/completions"); n != 2 {
		t.Errorf("upstream requests = %d, want 2 (served + quota rejection, no retry behind the announced 60 s reset)", n)
	}
}

// 14. Proxy quota class: an insufficient_quota rejection cools the account
// directly. The first call surfaces 402; the next call is refused by the
// health gate with the OpenAI overload mapping (503 + Retry-After) and never
// reaches the upstream again.
func TestMatrix_ChatClient_InsufficientQuotaCooldown(t *testing.T) {
	ctx := context.Background()
	up := upstream{ID: "quota-cooldown-up", Type: "custom-openai"}
	e := newMatrixEnv(t, up)
	e.ms.Upsert(t, map[string]any{
		"httpRequest": map[string]any{"method": "POST", "path": "/v1/chat/completions"},
		"httpResponse": map[string]any{
			"statusCode": 402,
			"headers": []map[string]any{{
				"name": "Retry-After", "values": []string{"30"},
			}},
			"body": `{"error":{"message":"quota exhausted","type":"insufficient_quota","code":"insufficient_quota"}}`,
		},
	})

	_, err := e.openai.Chat.Completions.New(ctx, openaiclient.ChatCompletionNewParams{
		Model:    up.ID,
		Messages: []openaiclient.ChatCompletionMessageParamUnion{openaiclient.UserMessage("hi")},
	})
	if err == nil {
		t.Fatal("client received an answer from an insufficient-quota upstream")
	}
	var firstErr *openaiclient.Error
	if !asOpenAIError(err, &firstErr) {
		t.Fatalf("first error = %v, want the SDK's API error", err)
	}
	if firstErr.StatusCode != http.StatusPaymentRequired {
		t.Errorf("first status = %d, want 402", firstErr.StatusCode)
	}
	if !e.health.Cooldown(up.ID) {
		t.Error("account is not in a quota cooldown after insufficient_quota")
	}
	if got := e.health.State(up.ID).Reason; got != "quota_exhausted" {
		t.Errorf("health reason = %q, want quota_exhausted", got)
	}

	_, err = e.openai.Chat.Completions.New(ctx, openaiclient.ChatCompletionNewParams{
		Model:    up.ID,
		Messages: []openaiclient.ChatCompletionMessageParamUnion{openaiclient.UserMessage("again")},
	})
	if err == nil {
		t.Fatal("client received an answer while the account was quota-cooled")
	}
	var secondErr *openaiclient.Error
	if !asOpenAIError(err, &secondErr) {
		t.Fatalf("second error = %v, want the SDK's API error", err)
	}
	if secondErr.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("second status = %d, want 503 (overload mapping)", secondErr.StatusCode)
	}
	if secondErr.Response == nil || secondErr.Response.Header.Get("Retry-After") == "" {
		t.Error("second response carries no Retry-After header")
	}
	if n := countUpstreamRequests(t, e, "/v1/chat/completions"); n != 1 {
		t.Errorf("upstream requests = %d, want exactly 1 (the cooled account must not be re-paid)", n)
	}

	// The log rows tell the story: the 402 attempt, then the 503 gate refusal.
	deadline := time.Now().Add(5 * time.Second)
	var rows []storage.RequestLog
	for time.Now().Before(deadline) {
		got, listErr := e.store.ListRequestLogs(ctx, 8)
		if listErr != nil {
			t.Fatalf("list request logs: %v", listErr)
		}
		rows = rowsOf(got, up.ID)
		if len(rows) >= 2 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(rows) < 2 {
		t.Fatalf("request rows = %d, want at least 2 (402 then 503)", len(rows))
	}
	if rows[0].Status != http.StatusServiceUnavailable || rows[1].Status != http.StatusPaymentRequired {
		t.Errorf("row statuses (newest first) = %d,%d, want 503,402", rows[0].Status, rows[1].Status)
	}
}

// asOpenAIError is errors.As without importing errors twice in this file.
func asOpenAIError(err error, target **openaiclient.Error) bool {
	for err != nil {
		if e, ok := err.(*openaiclient.Error); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
