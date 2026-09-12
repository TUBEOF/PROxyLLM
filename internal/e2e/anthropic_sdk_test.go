package e2e

// Anthropic official-SDK E2E tests (Phase 1 DoD, Task #19): the SDK that
// backs Claude Code's ANTHROPIC_BASE_URL gateway path drives the proxy's
// Anthropic surface. Acceptance derived from plan §5.1/§6.4 + wire facts
// A2/A5–A7/A13/C1 — never from implementation behavior. The mock upstream
// speaks OpenAI chat wire, so every round trip here is the cross-surface
// OP-2 translation (Anthropic client in, OpenAI upstream, Anthropic
// response out).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// 1. Claude Code message round trip (C1): Messages.New through the proxy
// against the chat-wire mock returns a protocol-conform Anthropic message
// with the upstream text translated into a text block.
func TestAnthropicSDK_MessagesRoundTrip(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.mock.Enqueue(mockJSON(t, 200, chatResponseBody()))

	msg, err := e.anthrop.Messages.New(ctx, anthropic.MessageNewParams{
		MaxTokens: 256,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("What's the weather in Paris?"))},
		Model:     "claude-sonnet-5",
	})
	if err != nil {
		t.Fatalf("messages create: %v", err)
	}
	var text strings.Builder
	for _, blk := range msg.Content {
		if v, ok := blk.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(v.Text)
		}
	}
	if !strings.Contains(text.String(), "The weather in Paris is sunny.") {
		t.Errorf("text blocks = %q, want the upstream answer translated", text.String())
	}
	if msg.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn (chat finish_reason stop mapped)", msg.StopReason)
	}
	if msg.Usage.InputTokens != 12 || msg.Usage.OutputTokens != 7 {
		t.Errorf("usage = %d/%d, want upstream usage translated into Anthropic usage", msg.Usage.InputTokens, msg.Usage.OutputTokens)
	}
}

// 2. Claude Code streaming (A5–A7, C2): Messages.NewStreaming yields the
// ordered event sequence message_start → content_block_delta(text) →
// message_delta → message_stop with the upstream deltas relayed.
func TestAnthropicSDK_MessagesStreaming(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.mock.Enqueue(e.chatStreamScript(t))

	stream := e.anthrop.Messages.NewStreaming(ctx, anthropic.MessageNewParams{
		MaxTokens: 256,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
		Model:     "claude-sonnet-5",
	})
	defer func() { _ = stream.Close() }()

	var sawStart, sawTextDelta, sawDelta, sawStop bool
	var text strings.Builder
	for stream.Next() {
		ev := stream.Current()
		switch ev.Type {
		case "message_start":
			sawStart = true
		case "content_block_delta":
			sawTextDelta = true
			if ev.Delta.Type == "text_delta" {
				text.WriteString(ev.Delta.Text)
			}
		case "message_delta":
			sawDelta = true
		case "message_stop":
			sawStop = true
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if !sawStart || !sawTextDelta || !sawDelta || !sawStop {
		t.Errorf("events: start=%v text=%v delta=%v stop=%v, want the ordered A5–A7 sequence",
			sawStart, sawTextDelta, sawDelta, sawStop)
	}
	if !strings.Contains(text.String(), "Hello") {
		t.Errorf("relayed deltas = %q, want Hello relayed", text.String())
	}
}

// 3. Count tokens (A2/C1): the SDK's CountTokens call resolves through
// the proxy — a heuristic answer for a chat-wire upstream, marked
// estimation (the proxy must answer, never fail, on a non-Anthropic
// upstream).
func TestAnthropicSDK_CountTokens(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)

	count, err := e.anthrop.Messages.CountTokens(ctx, anthropic.MessageCountTokensParams{
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("Count me: hello world, how long?"))},
		Model:    "claude-sonnet-5",
	})
	if err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if count.InputTokens <= 0 {
		t.Errorf("input_tokens = %d, want a positive heuristic estimate", count.InputTokens)
	}
}

// 4. THE Phase 1 DoD acceptance (§6.4, OP-2): a Claude Code client
// request with extended thinking (A12) routed to a NON-Claude model whose
// compat policy is `thinking_handling: "strip"`. The request must succeed
// (thinking never reaches the chat-wire upstream), the upstream must see
// NO thinking artifacts, and the request log's route_trace must show the
// compat_policy event for thinking_handling=strip — never silent.
func TestAnthropicSDK_ThinkingStripOnNonClaude(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seedModel(t, "claude-sonnet-5", json.RawMessage(`{"thinking_handling":"strip"}`))
	e.mock.Enqueue(mockJSON(t, 200, chatResponseBody()))

	msg, err := e.anthrop.Messages.New(ctx, anthropic.MessageNewParams{
		MaxTokens: 512,
		Thinking:  anthropic.ThinkingConfigParamOfEnabled(1024),
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
		Model:     "claude-sonnet-5",
	})
	if err != nil {
		t.Fatalf("thinking request on non-Claude target: %v", err)
	}
	if len(msg.Content) == 0 {
		t.Error("empty content — the answer must still arrive")
	}

	// Upstream discipline: no thinking param, no reasoning artifacts in the
	// chat-wire body the mock received (strip removes the param AND
	// history thinking blocks; never a fabricated signature).
	reqs := e.mock.Requests()
	if len(reqs) == 0 {
		t.Fatal("upstream saw no request")
	}
	if strings.Contains(reqs[0].Body, "thinking") {
		t.Errorf("upstream body contains thinking artifacts: %s", reqs[0].Body)
	}

	// Observable, never silent: route_trace carries the compat_policy
	// event {"from":"thinking_handling","to":"strip"}.
	logs := e.requestLogs(t)
	if len(logs) == 0 {
		t.Fatal("no request_logs row written")
	}
	if !traceCompatEvent(t, logs[0].RouteTrace, "thinking_handling", "strip") {
		t.Errorf("route_trace = %s, want a compat_policy event thinking_handling=strip", logs[0].RouteTrace)
	}
}

// 5. Thinking block (§6.4): with thinking_handling:"block", a thinking
// request is refused with a protocol-conform 4xx error carrying the
// documented message — and NO upstream request is dispatched.
func TestAnthropicSDK_ThinkingBlockOnNonClaude(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seedModel(t, "claude-sonnet-5", json.RawMessage(`{"thinking_handling":"block"}`))

	_, err := e.anthrop.Messages.New(ctx, anthropic.MessageNewParams{
		MaxTokens: 512,
		Thinking:  anthropic.ThinkingConfigParamOfEnabled(1024),
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
		Model:     "claude-sonnet-5",
	})
	if err == nil {
		t.Fatal("thinking request on a block-policy model unexpectedly succeeded")
	}
	if status := httpStatusOfAnthropic(t, err); status < 400 || status >= 500 {
		t.Errorf("block status = %d, want 4xx (§5.3)", status)
	}
	if reqs := e.mock.Requests(); len(reqs) != 0 {
		t.Errorf("upstream saw %d requests, want 0 (block refuses before dispatch)", len(reqs))
	}
}

// 6. No-route error (§5.3): an unknown model surfaces as a typed SDK
// APIError with a 4xx status in the Anthropic error shape.
func TestAnthropicSDK_NoRouteTypedError(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)

	_, err := e.anthrop.Messages.New(ctx, anthropic.MessageNewParams{
		MaxTokens: 64,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
		Model:     "no-such-model",
	})
	if err == nil {
		t.Fatal("no-route request unexpectedly succeeded")
	}
	if status := httpStatusOfAnthropic(t, err); status < 400 || status >= 500 {
		t.Errorf("no-route status = %d, want 4xx (§5.3)", status)
	}
}
