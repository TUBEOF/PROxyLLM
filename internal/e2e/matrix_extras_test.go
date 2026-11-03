package e2e

// Matrix extras: the cells outside the pair cross product — the web-tool
// emulation flow (fully offline: MockServer serves the Firecrawl v2 wire as
// the web-tool backend), the Anthropic count_tokens surface, and the negative
// control that proves a wrong upstream shape cannot pass a cell silently.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	openaiclient "github.com/openai/openai-go/v3"
)

// TestMatrix_Meta_NegativeControl_BadUpstreamShape proves the cells have
// teeth: an upstream answering 200 with a body that is not the provider's
// wire shape must surface as a client error, never as a green answer.
func TestMatrix_Meta_NegativeControl_BadUpstreamShape(t *testing.T) {
	ctx := context.Background()
	up := upstream{ID: "bad-shape-up", Type: "ollama-cloud"}
	e := newMatrixEnv(t, up)
	e.ms.Upsert(t, map[string]any{
		"httpRequest":  map[string]any{"method": "POST", "path": "/v1/chat/completions"},
		"httpResponse": map[string]any{"statusCode": 200, "body": "{ not json"},
	})

	_, err := e.openai.Chat.Completions.New(ctx, openaiclient.ChatCompletionNewParams{
		Model:    up.ID,
		Messages: []openaiclient.ChatCompletionMessageParamUnion{openaiclient.UserMessage("hi")},
	})
	if err == nil {
		t.Fatal("a 200 answer whose body is not the chat wire shape must fail the client")
	}
}

// TestMatrix_WebEmulation_AnthropicClient_Search runs the emulated web_search
// flow end to end and offline: the Anthropic client declares the server tool,
// the simulated upstream calls it, the proxy answers it through the Firecrawl
// emulator — which MockServer also plays — re-dispatches, and the client
// receives the final answer plus the rendered server-tool blocks.
func TestMatrix_WebEmulation_AnthropicClient_Search(t *testing.T) {
	ctx := context.Background()
	up := upstream{ID: "anthropic-web", Type: "anthropic"}
	e := newMatrixEnvEmulated(t, up)

	// Round 1: the upstream model calls the emulated web_search tool. Exactly
	// once, so the follow-up dispatch matches the next expectation.
	e.ms.Upsert(t, map[string]any{
		"httpRequest": map[string]any{"method": "POST", "path": "/v1/messages"},
		"httpLlmResponse": map[string]any{
			"provider": "ANTHROPIC",
			"model":    up.ID,
			"completion": map[string]any{
				"stopReason": "tool_use",
				"toolCalls": []map[string]any{{
					"id":        "call_web_1",
					"name":      "web_search",
					"arguments": `{"query":"weather in Cologne"}`,
				}},
			},
		},
		"times": map[string]any{"remainingTimes": 1, "unlimited": false},
	})
	// The web-tool backend (Firecrawl v2 wire) answers the search.
	e.ms.Upsert(t, map[string]any{
		"httpRequest":  map[string]any{"method": "POST", "path": "/v2/search"},
		"httpResponse": map[string]any{"statusCode": 200, "body": `{"success":true,"data":{"web":[{"title":"Cologne forecast","url":"https://example.test/cologne","description":"12C sunny tomorrow"}]}}`},
	})
	// Round 2: the model answers with the search result.
	e.ms.Upsert(t, map[string]any{
		"httpRequest": map[string]any{"method": "POST", "path": "/v1/messages"},
		"httpLlmResponse": map[string]any{
			"provider":   "ANTHROPIC",
			"model":      up.ID,
			"completion": map[string]any{"text": "Tomorrow in Cologne: 12C sunny, see https://example.test/cologne"},
		},
	})

	msg, err := e.anthrop.Messages.New(ctx, anthropic.MessageNewParams{
		MaxTokens: 256,
		Model:     up.ID,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("weather in Cologne tomorrow?"))},
		Tools: []anthropic.ToolUnionParam{{
			OfWebSearchTool20250305: &anthropic.WebSearchTool20250305Param{},
		}},
	})
	if err != nil {
		t.Fatalf("messages create: %v", err)
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal messages result: %v", err)
	}
	if !strings.Contains(string(raw), "12C sunny") {
		t.Fatalf("client answer carries no final text: %s", raw)
	}
	if !strings.Contains(string(raw), "web_search_tool_result") && !strings.Contains(string(raw), "server_tool_use") {
		t.Fatalf("client answer carries no rendered server-tool block: %s", raw)
	}

	// The upstream saw the emulated tool declared, then the tool result ride
	// back into the re-dispatched conversation.
	reqs := e.ms.Requests(t)
	var upstreamMessages int
	var redispatch map[string]any
	for _, r := range reqs {
		if r.Path != "/v1/messages" {
			continue
		}
		upstreamMessages++
		redispatch = upstreamJSON(t, r)
	}
	if upstreamMessages != 2 {
		t.Fatalf("upstream /v1/messages requests = %d, want 2 (call + re-dispatch)", upstreamMessages)
	}
	redispatchRaw, _ := json.Marshal(redispatch)
	if !strings.Contains(string(redispatchRaw), "example.test/cologne") {
		t.Errorf("the re-dispatched conversation carries no search result: %s", redispatchRaw)
	}

	// The web-tool backend received the query with the account's Bearer key.
	var searchReq *struct {
		Body    map[string]any
		Headers map[string][]string
	}
	for _, r := range reqs {
		if r.Path == "/v2/search" {
			body := upstreamJSON(t, r)
			headers := r.Headers
			searchReq = &struct {
				Body    map[string]any
				Headers map[string][]string
			}{Body: body, Headers: headers}
		}
	}
	if searchReq == nil {
		t.Fatal("the web-tool backend received no /v2/search request")
	}
	if searchReq.Body["query"] != "weather in Cologne" {
		t.Errorf("web-tool query = %v, want the model's query", searchReq.Body["query"])
	}
	if auth := searchReq.Headers["Authorization"]; len(auth) == 0 || !strings.HasPrefix(auth[0], "Bearer ") {
		t.Errorf("web-tool Authorization = %v, want the account's Bearer key", auth)
	}
}

// TestMatrix_AnthropicClient_CountTokens covers the count_tokens surface: the
// proxy estimates locally, so no upstream call may happen.
func TestMatrix_AnthropicClient_CountTokens(t *testing.T) {
	ctx := context.Background()
	up := upstream{ID: "anthropic-count", Type: "anthropic"}
	e := newMatrixEnv(t, up)

	out, err := e.anthrop.Messages.CountTokens(ctx, anthropic.MessageCountTokensParams{
		Model:    up.ID,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("count these tokens please"))},
	})
	if err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if out.InputTokens <= 0 {
		t.Fatalf("input_tokens = %d, want a positive estimate", out.InputTokens)
	}
	if reqs := e.ms.Requests(t); len(reqs) != 0 {
		t.Fatalf("upstream requests = %d, want 0 (count_tokens is estimated locally)", len(reqs))
	}
}
