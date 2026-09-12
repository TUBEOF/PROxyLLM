package e2e

// OpenAI official-SDK E2E tests (Phase 1 DoD, Task #19): openai-go v3
// drives chat completions, Responses, and models discovery against the
// assembled proxy. Acceptance derived from plan §5.1 (surface contract),
// O5–O8/O9/O10 (wire facts), and the golden fixtures — NOT from
// implementation behavior. The mock upstream speaks OpenAI chat wire, so
// the chat surface is a straight relay and the Responses surface proves
// the bidirectional IR translation.

import (
	"context"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

// 1. Chat round trip: the official SDK client creates a chat completion
// through the proxy against the scripted mock upstream and relays the
// assistant text (O2).
func TestOpenAISDK_ChatRoundTrip(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.mock.Enqueue(mockJSON(t, 200, chatResponseBody()))

	cc, err := e.openai.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:     "gpt-5",
		Messages:  []openai.ChatCompletionMessageParamUnion{openai.UserMessage("What's the weather in Paris?")},
		MaxTokens: openai.Int(64),
	})
	if err != nil {
		t.Fatalf("chat completion: %v", err)
	}
	if cc.ID != "chatcmpl-e2e" {
		t.Errorf("id = %q, want the scripted chatcmpl-e2e", cc.ID)
	}
	if len(cc.Choices) != 1 || cc.Choices[0].Message.Content != "The weather in Paris is sunny." {
		t.Errorf("choices = %+v, want the scripted assistant text relayed", cc.Choices)
	}
	if cc.Usage.PromptTokens != 12 || cc.Usage.CompletionTokens != 7 {
		t.Errorf("usage = %d/%d, want the scripted 12/7 relayed", cc.Usage.PromptTokens, cc.Usage.CompletionTokens)
	}
}

// 2. Chat streaming: NewStreaming yields the relayed upstream deltas and
// the terminal usage frame (O4) — the SDK assembles the full text.
func TestOpenAISDK_ChatStreaming(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.mock.Enqueue(e.chatStreamScript(t))

	stream := e.openai.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{
		Model:    "gpt-5",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")},
	})
	defer func() { _ = stream.Close() }()

	var text strings.Builder
	for stream.Next() {
		chunk := stream.Current()
		for _, ch := range chunk.Choices {
			text.WriteString(ch.Delta.Content)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	joined := text.String()
	if !strings.Contains(joined, "Hello") || !strings.Contains(joined, " world") {
		t.Errorf("assembled stream text = %q, want Hello/world deltas relayed", joined)
	}
}

// 3. Responses API round trip: the official SDK creates a Response
// through the proxy; the proxy decodes Responses wire → IR, routes to the
// chat-wire mock, and re-encodes IR → Responses (O5).
func TestOpenAISDK_ResponsesRoundTrip(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.mock.Enqueue(mockJSON(t, 200, chatResponseBody()))

	resp, err := e.openai.Responses.New(ctx, responses.ResponseNewParams{
		Model: "gpt-5",
		Input: responses.ResponseNewParamsInputUnion{OfString: openai.String("What's the weather in Paris?")},
	})
	if err != nil {
		t.Fatalf("responses create: %v", err)
	}
	if !strings.Contains(resp.OutputText(), "The weather in Paris is sunny.") {
		t.Errorf("output_text = %q, want the upstream answer translated into the Responses shape", resp.OutputText())
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 7 {
		t.Errorf("usage = %d/%d, want upstream usage mapped into Responses usage", resp.Usage.InputTokens, resp.Usage.OutputTokens)
	}
}

// 4. Responses streaming: the SDK stream surfaces response.created →
// output_text.delta → response.completed with terminal usage (O7).
func TestOpenAISDK_ResponsesStreaming(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.mock.Enqueue(e.chatStreamScript(t))

	stream := e.openai.Responses.NewStreaming(ctx, responses.ResponseNewParams{
		Model: "gpt-5",
		Input: responses.ResponseNewParamsInputUnion{OfString: openai.String("hi")},
	})
	defer func() { _ = stream.Close() }()

	var sawCreated, sawCompleted, sawDelta bool
	var text strings.Builder
	for stream.Next() {
		ev := stream.Current()
		switch ev.Type {
		case "response.created":
			sawCreated = true
		case "response.output_text.delta":
			sawDelta = true
			text.WriteString(ev.Delta)
		case "response.completed":
			sawCompleted = true
			// Terminal usage comes from the golden stream's final usage frame
			// (openai/chat-stream.sse: 1250 input / 128 output) — the same
			// contract TestResponses_StreamRelay pins.
			if ev.Response.Usage.InputTokens != 1250 {
				t.Errorf("terminal input tokens = %d, want the upstream terminal usage", ev.Response.Usage.InputTokens)
			}
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if !sawCreated || !sawDelta || !sawCompleted {
		t.Errorf("events: created=%v delta=%v completed=%v, want the full O7 semantic sequence", sawCreated, sawDelta, sawCompleted)
	}
	if !strings.Contains(text.String(), "Hello") {
		t.Errorf("relayed deltas = %q, want Hello relayed", text.String())
	}
}

// 5. Models discovery: Models.List resolves the stored model rows through
// the proxy in the O9 shape (the SDK's typed page decode proves the
// shape).
func TestOpenAISDK_ModelsList(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seedModel(t, "gpt-5", nil)

	page, err := e.openai.Models.List(ctx)
	if err != nil {
		t.Fatalf("models list: %v", err)
	}
	var sawGpt5 bool
	for _, m := range page.Data {
		if m.ID == "gpt-5" {
			sawGpt5 = true
		}
	}
	if !sawGpt5 {
		t.Errorf("models page = %+v, want the seeded gpt-5 row", page.Data)
	}
}

// 6. No-route error surfaces as a typed SDK APIError with a 4xx status
// (§5.3, O10) — never a 5xx or a transport error.
func TestOpenAISDK_NoRouteTypedError(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)

	_, err := e.openai.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:     "no-such-model",
		Messages:  []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")},
		MaxTokens: openai.Int(16),
	})
	if err == nil {
		t.Fatal("no-route request unexpectedly succeeded")
	}
	if status := httpNoRouteStatus(t, err); status < 400 || status >= 500 {
		t.Errorf("no-route status = %d, want 4xx (§5.3)", status)
	}
}
