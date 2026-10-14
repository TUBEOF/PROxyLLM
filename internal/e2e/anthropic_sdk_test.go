package e2e

// Anthropic official-SDK E2E tests: the SDK that
// backs Claude Code's ANTHROPIC_BASE_URL gateway path drives the proxy's
// Anthropic surface. Acceptance derived from the surface contracts, the
// compat-policy shape, and the Anthropic wire facts — never from
// implementation behavior. The mock upstream
// speaks OpenAI chat wire, so every round trip here is the cross-surface
// translation (Anthropic client in, OpenAI upstream, Anthropic
// response out).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// 1. Claude Code message round trip: Messages.New through the proxy
// against the chat-wire mock returns a protocol-conform Anthropic message
// with the golden answer translated — text block, tool_use block, stop_reason
// tool_use, usage — proving the full cross-surface translation.
func TestAnthropicSDK_MessagesRoundTrip(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.mock.Enqueue(mockJSON(t, 200, chatResponseBody(t)))

	msg, err := e.anthrop.Messages.New(ctx, anthropic.MessageNewParams{
		MaxTokens: 256,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("What's the weather in Paris?"))},
		Model:     "claude-sonnet-5",
	})
	if err != nil {
		t.Fatalf("messages create: %v", err)
	}
	var text strings.Builder
	var tool *anthropic.ToolUseBlock
	for _, blk := range msg.Content {
		if v, ok := blk.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(v.Text)
		}
		if v, ok := blk.AsAny().(anthropic.ToolUseBlock); ok {
			tool = &v
		}
	}
	if !strings.Contains(text.String(), "I'll check the weather for you.") {
		t.Errorf("text blocks = %q, want the golden answer translated", text.String())
	}
	// Golden tool call translated into a tool_use block: id, name,
	// input JSON — chat tool_calls map bidirectionally.
	if tool == nil {
		t.Fatalf("no tool_use block, want the golden tool call translated")
	}
	if tool.ID != "call_abc123" || tool.Name != "get_weather" {
		t.Errorf("tool_use = %s/%s, want call_abc123/get_weather", tool.ID, tool.Name)
	}
	if !strings.Contains(string(tool.Input), "San Francisco, CA") {
		t.Errorf("tool_use input = %s, want the golden location translated", tool.Input)
	}
	if msg.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use (chat finish_reason tool_calls mapped)", msg.StopReason)
	}
	if msg.Usage.InputTokens != 1250 || msg.Usage.OutputTokens != 77 {
		t.Errorf("usage = %d/%d, want the golden usage translated into Anthropic usage", msg.Usage.InputTokens, msg.Usage.OutputTokens)
	}
}

// 2. Claude Code streaming: Messages.NewStreaming yields the
// ordered event sequence message_start → content_block_delta(text) →
// message_delta → message_stop with the upstream deltas relayed. Ordering is
// asserted with a phase counter so the sequence cannot pass as an unordered
// set.
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

	const (
		phaseWantStart = 1
		phaseWantText  = 2
		phaseWantDelta = 3
		phaseWantStop  = 4
	)
	var phase int
	var text strings.Builder
	for stream.Next() {
		ev := stream.Current()
		switch ev.Type {
		case "message_start":
			if phase < phaseWantStart {
				phase = phaseWantStart
			}
		case "content_block_delta":
			if phase >= phaseWantStart && phase < phaseWantText {
				phase = phaseWantText
			}
			if ev.Delta.Type == "text_delta" {
				text.WriteString(ev.Delta.Text)
			}
		case "message_delta":
			if phase >= phaseWantText && phase < phaseWantDelta {
				phase = phaseWantDelta
			}
		case "message_stop":
			if phase >= phaseWantDelta {
				phase = phaseWantStop
			}
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if phase != phaseWantStop {
		t.Errorf("event ordering phase = %d, want %d (message_start → content_block_delta → message_delta → message_stop)", phase, phaseWantStop)
	}
	if !strings.Contains(text.String(), "Hello") {
		t.Errorf("relayed deltas = %q, want Hello relayed", text.String())
	}
}

// 3. Count tokens: the SDK's CountTokens call resolves through
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
	// The heuristic answer must be marked as an estimation (never
	// silently native-looking on a non-Anthropic upstream). MessageTokensCount
	// carries the raw response, so the marker is pinned on the wire itself.
	if !strings.Contains(count.RawJSON(), `"estimation":"heuristic"`) {
		t.Errorf("count response = %s, want the estimation:heuristic marker", count.RawJSON())
	}
}

// 4. THE acceptance case (cross-surface compat policy): a Claude Code client
// request with extended thinking routed to a NON-Claude model whose
// compat policy is `thinking_handling: "strip"`. The request must succeed
// (thinking never reaches the chat-wire upstream), the upstream must see
// NO thinking artifacts, and the request log's route_trace must show the
// compat_policy event for thinking_handling=strip — never silent.
func TestAnthropicSDK_ThinkingStripOnNonClaude(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seedModel(t, "claude-sonnet-5", json.RawMessage(`{"thinking_handling":"strip"}`))
	e.mock.Enqueue(mockJSON(t, 200, chatResponseBody(t)))

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

// 5. Thinking block: with thinking_handling:"block", a thinking
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
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("SDK returned %v, want an API error", err)
	}
	if apiErr.StatusCode < 400 || apiErr.StatusCode >= 500 {
		t.Errorf("block status = %d, want 4xx", apiErr.StatusCode)
	}
	// The error envelope must carry the Anthropic error type the
	// proxy maps ErrThinkingBlocked to (400 invalid_request_error), so
	// Claude Code surfaces a protocol-conform refusal, not a generic 4xx.
	if got := string(apiErr.Type()); got != "invalid_request_error" {
		t.Errorf("block error type = %q, want invalid_request_error", got)
	}
	if reqs := e.mock.Requests(); len(reqs) != 0 {
		t.Errorf("upstream saw %d requests, want 0 (block refuses before dispatch)", len(reqs))
	}
}

// 6. No-route error: an unknown model surfaces as a typed SDK
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
		t.Errorf("no-route status = %d, want 4xx", status)
	}
}
