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
// through the proxy against the scripted mock upstream and relays the golden
// answer — text, tool calls, finish reason, usage (O2/O3/O12) — verbatim.
func TestOpenAISDK_ChatRoundTrip(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.mock.Enqueue(mockJSON(t, 200, chatResponseBody(t)))

	cc, err := e.openai.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:     "gpt-5",
		Messages:  []openai.ChatCompletionMessageParamUnion{openai.UserMessage("What's the weather in Paris?")},
		MaxTokens: openai.Int(64),
	})
	if err != nil {
		t.Fatalf("chat completion: %v", err)
	}
	if cc.ID != "chatcmpl-abc123" {
		t.Errorf("id = %q, want the golden chatcmpl-abc123", cc.ID)
	}
	if len(cc.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(cc.Choices))
	}
	if cc.Choices[0].Message.Content != "I'll check the weather for you." {
		t.Errorf("content = %q, want the golden assistant text relayed", cc.Choices[0].Message.Content)
	}
	// Golden tool call (O3): id, function name, JSON-string arguments.
	calls := cc.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want the golden single tool call", len(calls))
	}
	if calls[0].ID != "call_abc123" || calls[0].Function.Name != "get_weather" {
		t.Errorf("tool call = %s/%s, want call_abc123/get_weather", calls[0].ID, calls[0].Function.Name)
	}
	if !strings.Contains(calls[0].Function.Arguments, "San Francisco, CA") {
		t.Errorf("tool arguments = %q, want the golden location relayed", calls[0].Function.Arguments)
	}
	if cc.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls (golden)", cc.Choices[0].FinishReason)
	}
	if cc.Usage.PromptTokens != 1250 || cc.Usage.CompletionTokens != 77 {
		t.Errorf("usage = %d/%d, want the golden 1250/77 relayed", cc.Usage.PromptTokens, cc.Usage.CompletionTokens)
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
	e.mock.Enqueue(mockJSON(t, 200, chatResponseBody(t)))

	resp, err := e.openai.Responses.New(ctx, responses.ResponseNewParams{
		Model: "gpt-5",
		Input: responses.ResponseNewParamsInputUnion{OfString: openai.String("What's the weather in Paris?")},
	})
	if err != nil {
		t.Fatalf("responses create: %v", err)
	}
	if !strings.Contains(resp.OutputText(), "I'll check the weather for you.") {
		t.Errorf("output_text = %q, want the golden answer translated into the Responses shape", resp.OutputText())
	}
	if resp.Usage.InputTokens != 1250 || resp.Usage.OutputTokens != 77 {
		t.Errorf("usage = %d/%d, want the golden usage mapped into Responses usage", resp.Usage.InputTokens, resp.Usage.OutputTokens)
	}
}

// 4. Responses streaming: the SDK stream surfaces the ordered O7 semantic
// sequence response.created → output_text.delta → response.completed with
// terminal usage, AND the golden stream's tool_calls translated into
// function_call output items (arguments deltas assembled, call id/name
// carried).
func TestOpenAISDK_ResponsesStreaming(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.mock.Enqueue(e.chatStreamScript(t))

	stream := e.openai.Responses.NewStreaming(ctx, responses.ResponseNewParams{
		Model: "gpt-5",
		Input: responses.ResponseNewParamsInputUnion{OfString: openai.String("hi")},
	})
	defer func() { _ = stream.Close() }()

	// Ordering is asserted with a phase counter: each milestone must first
	// appear AFTER its predecessor, so the O7 sequence cannot pass as a set.
	const (
		phaseWantCreated = 1
		phaseWantText    = 2
		phaseWantArgs    = 3
		phaseWantDone    = 4
	)
	var phase int
	var text, callArgs strings.Builder
	var sawFunctionItem bool
	var callID, callName string
	for stream.Next() {
		ev := stream.Current()
		switch ev.Type {
		case "response.created":
			if phase < phaseWantCreated {
				phase = phaseWantCreated
			}
		case "response.output_text.delta":
			if phase >= phaseWantCreated && phase < phaseWantText {
				phase = phaseWantText
			}
			text.WriteString(ev.Delta)
		case "response.output_item.added":
			// Tool-call translation: the golden chat stream's tool_calls
			// surface as function_call items (flat CallID/Name/Arguments on
			// the SDK's ResponseOutputItemUnion).
			if ev.Item.Type == "function_call" {
				sawFunctionItem = true
				callID, callName = ev.Item.CallID, ev.Item.Name
			}
		case "response.function_call_arguments.delta":
			if phase >= phaseWantText && phase < phaseWantArgs {
				phase = phaseWantArgs
			}
			// The event carries its fragment in the `delta` field (SDK
			// ResponseFunctionCallArgumentsDeltaEvent), same as text deltas.
			callArgs.WriteString(ev.Delta)
		case "response.completed":
			if phase >= phaseWantArgs {
				phase = phaseWantDone
			}
			// Terminal usage from the golden stream's final usage frame
			// (openai/chat-stream.sse: 1250 input / 128 output) — the same
			// contract TestResponses_StreamRelay pins.
			if ev.Response.Usage.InputTokens != 1250 || ev.Response.Usage.OutputTokens != 128 {
				t.Errorf("terminal usage = %d/%d, want the golden 1250/128", ev.Response.Usage.InputTokens, ev.Response.Usage.OutputTokens)
			}
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if phase != phaseWantDone {
		t.Errorf("event ordering phase = %d, want %d (created → text → args → completed)", phase, phaseWantDone)
	}
	if !strings.Contains(text.String(), "Hello") {
		t.Errorf("relayed deltas = %q, want Hello relayed", text.String())
	}
	if !sawFunctionItem || callID != "call_abc123" || callName != "get_weather" {
		t.Errorf("function_call item = %v (%s/%s), want call_abc123/get_weather translated from the golden tool_calls", sawFunctionItem, callID, callName)
	}
	if !strings.Contains(callArgs.String(), "ation") || !strings.Contains(callArgs.String(), "SF") {
		t.Errorf("assembled function arguments = %q, want the golden argument fragments relayed", callArgs.String())
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
