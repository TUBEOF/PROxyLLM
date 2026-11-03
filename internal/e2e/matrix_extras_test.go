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
	"github.com/openai/openai-go/v3/responses"

	"github.com/tubeof/proxyllm/internal/storage"
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

// TestMatrix_WebEmulation_InjectWebTools_PlainClient runs the plain-CLI
// scenario inject_web_tools exists for (codex started WITHOUT --search asks a
// weather question): the client declares NO tools, the model row's compat
// blob injects the emulated web families, the simulated upstream calls
// web_search, the emulator answers through MockServer's Firecrawl wire, and
// the final answer carries the source. The injected defs must be visible in
// the FIRST upstream round's wire bytes.
func TestMatrix_WebEmulation_InjectWebTools_PlainClient(t *testing.T) {
	ctx := context.Background()
	up := upstream{ID: "codex-plain", Type: "custom-openai"}
	e := newMatrixEnvEmulated(t, up)
	if err := e.store.PutModel(ctx, storage.Model{
		ID: up.ID, ProviderID: up.ID, Name: up.ID,
		Compat: json.RawMessage(`{"inject_web_tools":true}`),
	}); err != nil {
		t.Fatalf("seed model row: %v", err)
	}

	// Round 1: the upstream model calls the injected web_search tool. Exactly
	// once, so the follow-up dispatch matches the next expectation.
	e.ms.Upsert(t, map[string]any{
		"httpRequest": map[string]any{"method": "POST", "path": "/v1/chat/completions"},
		"httpLlmResponse": map[string]any{
			"provider": "OPENAI",
			"model":    up.ID,
			"completion": map[string]any{
				"stopReason": "tool_calls",
				"toolCalls": []map[string]any{{
					"id":        "call_inj_1",
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
		"httpRequest": map[string]any{"method": "POST", "path": "/v1/chat/completions"},
		"httpLlmResponse": map[string]any{
			"provider":   "OPENAI",
			"model":      up.ID,
			"completion": map[string]any{"text": "Tomorrow in Cologne: 12C sunny, see https://example.test/cologne"},
		},
	})

	resp, err := e.openai.Responses.New(ctx, responses.ResponseNewParams{
		Model: up.ID,
		Input: responses.ResponseNewParamsInputUnion{OfString: openaiclient.String("weather in Cologne tomorrow?")},
	})
	if err != nil {
		t.Fatalf("responses create: %v", err)
	}
	text := resp.OutputText()
	if !strings.Contains(text, "12C sunny") {
		t.Fatalf("final answer carries no result: %q", text)
	}
	if !strings.Contains(text, "https://example.test/cologne") {
		t.Fatalf("final answer carries no source citation: %q", text)
	}

	// The FIRST upstream round must carry the injected tool defs in its wire
	// bytes — a client that declared no tools still sees them offered.
	reqs := e.ms.Requests(t)
	var first map[string]any
	var chatCount int
	for _, r := range reqs {
		if r.Path != "/v1/chat/completions" {
			continue
		}
		chatCount++
		if first == nil {
			first = upstreamJSON(t, r)
		}
	}
	if chatCount != 2 {
		t.Fatalf("upstream /v1/chat/completions requests = %d, want 2 (call + re-dispatch)", chatCount)
	}
	raw, _ := json.Marshal(first["tools"])
	if first["tools"] == nil || !strings.Contains(string(raw), "web_search") || !strings.Contains(string(raw), "web_fetch") {
		t.Fatalf("round-1 upstream request declares tools = %s, want the injected web_search and web_fetch defs", raw)
	}

	// The web-tool backend received the model's query.
	var searchReq map[string]any
	for _, r := range reqs {
		if r.Path == "/v2/search" {
			searchReq = upstreamJSON(t, r)
		}
	}
	if searchReq == nil {
		t.Fatal("the web-tool backend received no /v2/search request")
	}
	if searchReq["query"] != "weather in Cologne" {
		t.Errorf("web-tool query = %v, want the model's query", searchReq["query"])
	}
}

// TestMatrix_WebEmulation_ChatClient_Search runs the client web-tool claim on
// the chat surface end to end: the OpenAI chat client declares an ordinary
// function tool (the shipped `client_web_tools` default name WebSearch), the
// claim serves it as the emulated web_search family, the simulated upstream
// calls it, the emulator answers through MockServer's Firecrawl wire, and the
// final answer text rides back. The client's own tool name must never reach
// the upstream — the wire carries the claimed family def.
func TestMatrix_WebEmulation_ChatClient_Search(t *testing.T) {
	ctx := context.Background()
	up := upstream{ID: "chat-claim-up", Type: "custom-openai"}
	e := newMatrixEnvEmulated(t, up)

	// Round 1: the upstream model calls the claimed web_search tool. Exactly
	// once, so the follow-up dispatch matches the next expectation.
	e.ms.Upsert(t, map[string]any{
		"httpRequest": map[string]any{"method": "POST", "path": "/v1/chat/completions"},
		"httpLlmResponse": map[string]any{
			"provider": "OPENAI",
			"model":    up.ID,
			"completion": map[string]any{
				"stopReason": "tool_calls",
				"toolCalls": []map[string]any{{
					"id":        "call_claim_1",
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
		"httpRequest": map[string]any{"method": "POST", "path": "/v1/chat/completions"},
		"httpLlmResponse": map[string]any{
			"provider":   "OPENAI",
			"model":      up.ID,
			"completion": map[string]any{"text": "Tomorrow in Cologne: 12C sunny, see https://example.test/cologne"},
		},
	})

	resp, err := e.openai.Chat.Completions.New(ctx, openaiclient.ChatCompletionNewParams{
		Model:    up.ID,
		Messages: []openaiclient.ChatCompletionMessageParamUnion{openaiclient.UserMessage("weather in Cologne tomorrow?")},
		Tools: []openaiclient.ChatCompletionToolUnionParam{{
			OfFunction: &openaiclient.ChatCompletionFunctionToolParam{
				Function: openaiclient.FunctionDefinitionParam{
					Name:       "WebSearch",
					Parameters: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}},
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("chat completion: %v", err)
	}
	if len(resp.Choices) == 0 || !strings.Contains(resp.Choices[0].Message.Content, "12C sunny") {
		t.Fatalf("client answer carries no final text: %+v", resp.Choices)
	}

	// The FIRST upstream round must carry the claimed family def, and never
	// the client's own tool name (the claim renames, the proxy answers).
	reqs := e.ms.Requests(t)
	var first map[string]any
	var chatCount int
	for _, r := range reqs {
		if r.Path != "/v1/chat/completions" {
			continue
		}
		chatCount++
		if first == nil {
			first = upstreamJSON(t, r)
		}
	}
	if chatCount != 2 {
		t.Fatalf("upstream /v1/chat/completions requests = %d, want 2 (call + re-dispatch)", chatCount)
	}
	raw, _ := json.Marshal(first["tools"])
	if first["tools"] == nil || !strings.Contains(string(raw), "web_search") || strings.Contains(string(raw), "WebSearch") {
		t.Fatalf("round-1 upstream request declares tools = %s, want the claimed web_search def and no client name", raw)
	}

	// The web-tool backend received the model's query.
	var searchReq map[string]any
	for _, r := range reqs {
		if r.Path == "/v2/search" {
			searchReq = upstreamJSON(t, r)
		}
	}
	if searchReq == nil {
		t.Fatal("the web-tool backend received no /v2/search request")
	}
	if searchReq["query"] != "weather in Cologne" {
		t.Errorf("web-tool query = %v, want the model's query", searchReq["query"])
	}
}

// TestMatrix_WebEmulation_AnthropicClient_Fetch runs the emulated web_fetch
// flow end to end: the Anthropic client declares the server tool, the user
// message carries the URL (the prior-context guard only fetches URLs that
// appear in the conversation), the simulated upstream calls web_fetch, the
// proxy scrapes through MockServer's Firecrawl v2 wire and re-dispatches, and
// the client receives the final answer plus the rendered document result.
func TestMatrix_WebEmulation_AnthropicClient_Fetch(t *testing.T) {
	ctx := context.Background()
	up := upstream{ID: "anthropic-fetch", Type: "anthropic"}
	e := newMatrixEnvEmulated(t, up)
	const pageURL = "https://example.test/cologne-forecast"

	// Round 1: the upstream model calls the emulated web_fetch tool. Exactly
	// once, so the follow-up dispatch matches the next expectation.
	e.ms.Upsert(t, map[string]any{
		"httpRequest": map[string]any{"method": "POST", "path": "/v1/messages"},
		"httpLlmResponse": map[string]any{
			"provider": "ANTHROPIC",
			"model":    up.ID,
			"completion": map[string]any{
				"stopReason": "tool_use",
				"toolCalls": []map[string]any{{
					"id":        "call_fetch_1",
					"name":      "web_fetch",
					"arguments": `{"url":"` + pageURL + `"}`,
				}},
			},
		},
		"times": map[string]any{"remainingTimes": 1, "unlimited": false},
	})
	// The web-tool backend (Firecrawl v2 wire) answers the scrape with the
	// documented 200 body: page markdown plus the page's own metadata.
	e.ms.Upsert(t, map[string]any{
		"httpRequest":  map[string]any{"method": "POST", "path": "/v2/scrape"},
		"httpResponse": map[string]any{"statusCode": 200, "body": `{"success":true,"data":{"markdown":"# Cologne forecast\n\n12C sunny tomorrow.","metadata":{"title":"Cologne forecast","url":"` + pageURL + `","statusCode":200}}}`},
	})
	// Round 2: the model answers with the fetched page's content.
	e.ms.Upsert(t, map[string]any{
		"httpRequest": map[string]any{"method": "POST", "path": "/v1/messages"},
		"httpLlmResponse": map[string]any{
			"provider":   "ANTHROPIC",
			"model":      up.ID,
			"completion": map[string]any{"text": "The Cologne forecast page says: 12C sunny tomorrow."},
		},
	})

	msg, err := e.anthrop.Messages.New(ctx, anthropic.MessageNewParams{
		MaxTokens: 512,
		Model:     up.ID,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("Summarize the page " + pageURL + " for me")),
		},
		Tools: []anthropic.ToolUnionParam{{
			OfWebFetchTool20250910: &anthropic.WebFetchTool20250910Param{},
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
		t.Fatalf("client answer carries no fetched content: %s", raw)
	}
	if !strings.Contains(string(raw), "web_fetch_tool_result") || !strings.Contains(string(raw), "server_tool_use") {
		t.Fatalf("client answer carries no rendered fetch blocks: %s", raw)
	}

	// The upstream saw the emulated tool declared, then the fetched page ride
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
	if !strings.Contains(string(redispatchRaw), "12C sunny") {
		t.Errorf("the re-dispatched conversation carries no fetched page: %s", redispatchRaw)
	}

	// The web-tool backend received the URL with the account's Bearer key.
	var scrapeReq map[string]any
	for _, r := range reqs {
		if r.Path == "/v2/scrape" {
			scrapeReq = upstreamJSON(t, r)
		}
	}
	if scrapeReq == nil {
		t.Fatal("the web-tool backend received no /v2/scrape request")
	}
	if scrapeReq["url"] != pageURL {
		t.Errorf("web-tool scrape url = %v, want the model's url", scrapeReq["url"])
	}
}

// TestMatrix_Thinking_AnthropicClient covers the thinking round trip: the
// Anthropic client enables extended thinking, the request must reach an
// upstream that itself accepts a thinking parameter, and the upstream's
// reasoning trace (MockServer's reasoningText completion) must surface to the
// client beside the final text — never dropped, never sent to a client that
// asked for none.
func TestMatrix_Thinking_AnthropicClient(t *testing.T) {
	ctx := context.Background()
	up := upstream{ID: "think-up", Type: "anthropic"}
	e := newMatrixEnv(t, up)
	if err := e.store.PutModel(ctx, storage.Model{
		ID: up.ID, ProviderID: up.ID, Name: up.ID,
		Capabilities: json.RawMessage(`{"thinking":true}`),
	}); err != nil {
		t.Fatalf("seed model row: %v", err)
	}
	e.ms.Upsert(t, map[string]any{
		"httpRequest": map[string]any{"method": "POST", "path": "/v1/messages"},
		"httpLlmResponse": map[string]any{
			"provider": "ANTHROPIC",
			"model":    up.ID,
			"completion": map[string]any{
				"reasoningText": "The question needs a forecast lookup, so I reason over it first.",
				"text":          "It is sunny.",
			},
		},
	})

	msg, err := e.anthrop.Messages.New(ctx, anthropic.MessageNewParams{
		MaxTokens: 2048,
		Model:     up.ID,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("what is the weather?"))},
		Thinking:  anthropic.ThinkingConfigParamOfEnabled(1024),
	})
	if err != nil {
		t.Fatalf("messages create: %v", err)
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal messages result: %v", err)
	}
	if !strings.Contains(string(raw), "The question needs a forecast lookup") {
		t.Fatalf("client answer carries no reasoning trace: %s", raw)
	}
	if !strings.Contains(string(raw), "It is sunny.") {
		t.Fatalf("client answer carries no final text: %s", raw)
	}
	if strings.Contains(string(raw), "redacted_thinking") {
		t.Fatalf("client answer carries a fabricated redaction: %s", raw)
	}

	// The upstream request carried the thinking parameter (type enabled with
	// a positive budget) — the client's intent survived the proxy.
	var body map[string]any
	for _, r := range e.ms.Requests(t) {
		if r.Path == "/v1/messages" {
			body = upstreamJSON(t, r)
		}
	}
	if body == nil {
		t.Fatal("the proxy dispatched no upstream request")
	}
	th, ok := body["thinking"].(map[string]any)
	if !ok || th["type"] != "enabled" {
		t.Fatalf("upstream thinking = %v, want type enabled", body["thinking"])
	}
	if budget, _ := th["budget_tokens"].(float64); budget <= 0 {
		t.Errorf("upstream budget_tokens = %v, want a positive budget", th["budget_tokens"])
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
