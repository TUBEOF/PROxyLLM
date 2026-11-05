package e2e

// Matrix extras: the cells outside the pair cross product — the web-tool
// emulation flow (fully offline: MockServer serves the Firecrawl v2 wire as
// the web-tool backend), the Anthropic count_tokens surface, the codex OAuth
// upstream, and the negative control that proves a wrong upstream shape
// cannot pass a cell silently.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	openaiclient "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"

	"github.com/tubeof/proxyllm/internal/provider/codex"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/testutil/mockserver"
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

// recordedHeader reads one header off a recorded request case-insensitively
// (MockServer echoes the casing it received).
func recordedHeader(r mockserver.Recorded, name string) []string {
	for k, v := range r.Headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return nil
}

// TestMatrix_Upstream_CodexOauth binds the codex-oauth upstream cell: a chat
// request routes to a codex-oauth account whose seeded access token has
// EXPIRED, so the proxy must rotate against the issuer first — the JSON
// refresh grant to MockServer's scripted /oauth/token — and then carry the
// OAuth-minted bearer, plus the account id lifted from the rotated id_token's
// claim, on the ChatGPT-backend request at /codex/responses. The second
// request reuses the minted token without a second issuer round trip. A
// fresh env, store and MockServer log per run keep the flow deterministic
// under -count=2.
func TestMatrix_Upstream_CodexOauth(t *testing.T) {
	ctx := context.Background()
	up := upstream{ID: "codex-oauth-up", Type: "codex-oauth"}
	e := newMatrixEnv(t, up)

	const (
		mintedAccess   = "codex-minted-access"
		rotatedRefresh = "codex-rotated-refresh"
		accountID      = "acct-matrix-codex"
	)
	// A structurally valid id_token whose payload names the account (test
	// fixture, never a credential).
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"chatgpt_account_id":"` + accountID + `"}`))
	idToken := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + payload + ".sig"

	// The issuer answers the refresh grant with the rotated token triple.
	e.ms.Upsert(t, map[string]any{
		"httpRequest":  map[string]any{"method": "POST", "path": "/oauth/token"},
		"httpResponse": map[string]any{"statusCode": 200, "body": `{"access_token":"` + mintedAccess + `","refresh_token":"` + rotatedRefresh + `","id_token":"` + idToken + `"}`},
	})
	// The ChatGPT backend answers the Responses wire — the codex provider
	// always dispatches streamed and assembles, so the completion streams.
	e.ms.Upsert(t, expectation("OPENAI_RESPONSES", up.ID, "/codex/responses",
		chatCompletion("codex oauth answer", true)))

	ask := func() string {
		resp, err := e.openai.Chat.Completions.New(ctx, openaiclient.ChatCompletionNewParams{
			Model:    up.ID,
			Messages: []openaiclient.ChatCompletionMessageParamUnion{openaiclient.UserMessage("hi")},
		})
		if err != nil {
			t.Fatalf("chat completion: %v", err)
		}
		if len(resp.Choices) == 0 {
			t.Fatal("client answer carries no choices")
		}
		return resp.Choices[0].Message.Content
	}
	for i := 1; i <= 2; i++ {
		if got := ask(); !strings.Contains(got, "codex oauth answer") {
			t.Fatalf("client answer %d = %q, want the upstream text", i, got)
		}
	}

	// The issuer saw EXACTLY one refresh grant — the first request rotated
	// the expired token, the second reused the minted one — and the backend
	// saw the minted bearer plus the id_token's account id on every request.
	var tokenReqs, backendReqs int
	for _, r := range e.ms.Requests(t) {
		switch r.Path {
		case "/oauth/token":
			tokenReqs++
			body := upstreamJSON(t, r)
			if body["grant_type"] != "refresh_token" {
				t.Errorf("refresh grant_type = %v, want refresh_token", body["grant_type"])
			}
			if body["refresh_token"] != codexOAuthSeedRefresh {
				t.Errorf("refresh_token = %v, want the seeded one", body["refresh_token"])
			}
			if body["client_id"] != codex.OAuthClientID {
				t.Errorf("refresh client_id = %v, want the pinned codex client id", body["client_id"])
			}
		case "/codex/responses":
			backendReqs++
			if auth := recordedHeader(r, "Authorization"); len(auth) == 0 || auth[0] != "Bearer "+mintedAccess {
				t.Errorf("backend Authorization = %v, want the OAuth-minted bearer", auth)
			}
			if acct := recordedHeader(r, "Chatgpt-Account-Id"); len(acct) == 0 || acct[0] != accountID {
				t.Errorf("backend Chatgpt-Account-Id = %v, want the id_token's account claim", acct)
			}
			body := upstreamJSON(t, r)
			if body["stream"] != true {
				t.Errorf("backend body stream = %v, want true (the backend has no non-streaming mode)", body["stream"])
			}
			if body["model"] != up.ID {
				t.Errorf("backend body model = %v, want %q", body["model"], up.ID)
			}
			if raw, _ := json.Marshal(body); strings.Contains(string(raw), codexOAuthSeedAccess) {
				t.Errorf("backend request leaks the seeded expired token: %s", raw)
			}
		}
	}
	if tokenReqs != 1 {
		t.Fatalf("issuer /oauth/token requests = %d, want 1 (rotation, then reuse)", tokenReqs)
	}
	if backendReqs != 2 {
		t.Fatalf("backend /codex/responses requests = %d, want 2", backendReqs)
	}

	// The rotation persisted: the store holds the minted pair, not the seeds.
	access, err := e.store.GetProviderToken(ctx, up.ID, codex.KindAccessToken)
	if err != nil || string(access.Secret) != mintedAccess {
		t.Errorf("stored access token = %v (err %v), want the minted one", access, err)
	}
	refresh, err := e.store.GetProviderToken(ctx, up.ID, codex.KindRefreshToken)
	if err != nil || string(refresh.Secret) != rotatedRefresh {
		t.Errorf("stored refresh token = %v (err %v), want the rotated one", refresh, err)
	}
}
