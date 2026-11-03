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

	openaiclient "github.com/openai/openai-go/v3"
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
