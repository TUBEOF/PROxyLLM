package openai_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/translate/ir"
	oa "github.com/tubeof/proxyllm/internal/translate/openai"
	"github.com/tubeof/proxyllm/internal/translate/testutil"
)

// Chat stream-encoder contract tests, RED-GREEN per Analysis F. The encoder
// must produce the chat-completions SSE wire format (facts O4, O12) such
// that the package's own stream decoder reconstructs the canonical events
// (round-trip discipline: the decoder is fixture-verified against
// developers.openai.com, so a passing round trip proves wire conformance).
//
// Spec-derived wire assertions follow the golden fixture
// testdata/golden/openai/chat-stream.sse. created is a runtime timestamp,
// so chunk assertions pin everything after it via the
// `"model":...,"choices":...` suffix, which also proves the fixture's field
// order id → object → created → model → choices.

// encodeChatAll runs evs through a fresh chat encoder over a buffer and
// returns the wire bytes.
func encodeChatAll(t *testing.T, evs []ir.StreamEvent) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := oa.NewStreamEncoder(&buf)
	for _, ev := range evs {
		if err := enc.Write(context.Background(), ev); err != nil {
			t.Fatalf("Write(%s): %v", ev.Type, err)
		}
	}
	return buf.Bytes()
}

// fullChatSequenceEvents is the canonical event sequence matching the golden
// openai/chat-stream.sse fixture (text + tool_use, index-assembled
// tool_calls per O4).
func fullChatSequenceEvents() []ir.StreamEvent {
	return []ir.StreamEvent{
		{Type: ir.Start, Response: &ir.Response{
			ID:    "chatcmpl-abc123",
			Model: "gpt-5",
		}},
		{Type: ir.BlockStart, Index: 0, Part: ir.TextPart{}},
		{Type: ir.Delta, Index: 0, Delta: ir.DeltaPayload{Kind: ir.DeltaText, Text: "Hello"}},
		{Type: ir.Delta, Index: 0, Delta: ir.DeltaPayload{Kind: ir.DeltaText, Text: " world"}},
		{Type: ir.BlockStop, Index: 0},
		{Type: ir.BlockStart, Index: 1, Part: ir.ToolCallPart{ID: "call_abc123", Name: "get_weather"}},
		{Type: ir.Delta, Index: 1, Delta: ir.DeltaPayload{Kind: ir.DeltaToolInput, PartialJSON: `{"location":`}},
		{Type: ir.Delta, Index: 1, Delta: ir.DeltaPayload{Kind: ir.DeltaToolInput, PartialJSON: `"San Francisco"}`}},
		{Type: ir.BlockStop, Index: 1},
		{Type: ir.MessageDelta, MessageDelta: &ir.MessageDeltaPayload{
			StopReason: ir.StopToolUse,
			Usage:      ir.Usage{InputTokens: 1250, OutputTokens: 128},
		}},
		{Type: ir.Done},
	}
}

func TestChatStreamEncode_FullSequenceRoundTrip(t *testing.T) {
	wire := encodeChatAll(t, fullChatSequenceEvents())

	// Round trip: the fixture-verified decoder must reconstruct the exact
	// canonical sequence.
	got := testutil.DrainStreamDecoder(t, oa.NewStreamDecoder(bytes.NewReader(wire)))
	want := fullChatSequenceEvents()
	testutil.AssertEventsEqual(t, "openai/chat-stream.sse", want, got)
}

// The wire shape must be data-only SSE with created/model on every chunk,
// JSON-null in-flight finish_reason, and the [DONE] sentinel (fixture lines
// 1, 4, 17) — not just round-trip semantically.
func TestChatStreamEncode_WireShape(t *testing.T) {
	evs := []ir.StreamEvent{
		{Type: ir.Start, Response: &ir.Response{ID: "chatcmpl-abc123", Model: "gpt-5"}},
		{Type: ir.Delta, Index: 0, Delta: ir.DeltaPayload{Kind: ir.DeltaText, Text: "Hello"}},
		{Type: ir.Done},
	}
	wire := string(encodeChatAll(t, evs))

	// created is a runtime timestamp, so assert the suffix from model on,
	// which pins field order and the model's presence on every chunk.
	for _, want := range []string{
		// First chunk: role assistant, empty content, null finish_reason
		// (fixture line 1: created+model precede choices).
		`"model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n",
		// Content chunk: minimal delta, still null finish_reason (line 4).
		`"model":"gpt-5","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}` + "\n\n",
		// Sentinel terminates the stream (line 17).
		"data: [DONE]\n\n",
	} {
		if !strings.Contains(wire, want) {
			t.Errorf("wire missing %q\ngot:\n%s", want, wire)
		}
	}
	// created must be a JSON number on every chunk (not the string the
	// suffix above can't see).
	if !strings.Contains(wire, `"object":"chat.completion.chunk","created":`) {
		t.Errorf("chunks must carry a numeric created field after object, got:\n%s", wire)
	}
	if strings.Count(wire, "data: ") != 3 {
		t.Errorf("wire must carry exactly 3 data lines, got:\n%s", wire)
	}
}

// Tool calls assemble by index (O4): id/name on the anchor chunk, argument
// fragments carry only index + function.arguments. The wire index is an
// encoder-owned counter starting at 0 (fixture line 7), independent of IR
// block indices.
func TestChatStreamEncode_ToolCallWireShape(t *testing.T) {
	evs := []ir.StreamEvent{
		{Type: ir.Start, Response: &ir.Response{ID: "chatcmpl-abc123", Model: "gpt-5"}},
		{Type: ir.BlockStart, Index: 1, Part: ir.ToolCallPart{ID: "call_abc123", Name: "get_weather"}},
		{Type: ir.Delta, Index: 1, Delta: ir.DeltaPayload{Kind: ir.DeltaToolInput, PartialJSON: `{"location":`}},
		{Type: ir.Done},
	}
	wire := string(encodeChatAll(t, evs))

	for _, want := range []string{
		// Tool-call anchor chunk: wire index 0, id/name ride the first
		// delta, arguments present but empty (fixture line 7).
		`"delta":{"tool_calls":[{"index":0,"id":"call_abc123","type":"function","function":{"name":"get_weather","arguments":""}}]}`,
		// Argument fragment: only wire index + function.arguments
		// (fixture line 9).
		`"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"location\":"}}]}`,
	} {
		if !strings.Contains(wire, want) {
			t.Errorf("wire missing %q\ngot:\n%s", want, wire)
		}
	}
}

// MessageDelta emits the deferred finish_reason chunk (empty delta object,
// fixture line 13) and then the usage chunk: a choice with an empty delta
// and null finish_reason PLUS the top-level usage object with explicit
// zero-value detail fields (fixture line 15).
func TestChatStreamEncode_FinishAndUsage(t *testing.T) {
	evs := []ir.StreamEvent{
		{Type: ir.Start, Response: &ir.Response{ID: "chatcmpl-abc123", Model: "gpt-5"}},
		{Type: ir.BlockStop, Index: 0},
		{Type: ir.MessageDelta, MessageDelta: &ir.MessageDeltaPayload{
			StopReason: ir.StopToolUse,
			Usage:      ir.Usage{InputTokens: 1250, OutputTokens: 128},
		}},
		{Type: ir.Done},
	}
	wire := string(encodeChatAll(t, evs))

	for _, want := range []string{
		// Deferred finish chunk: empty delta object + the stop reason
		// (fixture line 13).
		`"model":"gpt-5","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		// Usage chunk: regular choice with empty delta + null finish,
		// then top-level usage with explicit zero details (line 15).
		`"model":"gpt-5","choices":[{"index":0,"delta":{},"finish_reason":null}],"usage":{"prompt_tokens":1250,"completion_tokens":128,"total_tokens":1378,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}` + "\n\n",
	} {
		if !strings.Contains(wire, want) {
			t.Errorf("wire missing %q\ngot:\n%s", want, wire)
		}
	}
}

// ir.Error maps onto a chunk carrying a top-level error object (O10).
func TestChatStreamEncode_ErrorEvent(t *testing.T) {
	evs := []ir.StreamEvent{
		{Type: ir.Start, Response: &ir.Response{ID: "chatcmpl-1", Model: "gpt-5"}},
		{Type: ir.Error, Error: &ir.UpstreamError{Type: "server_error", Message: "boom"}},
	}
	wire := encodeChatAll(t, evs)

	// Round trip: the decoder opens the implicit text block on the first
	// chunk, so the terminal error comes back as the third event.
	got := testutil.DrainStreamDecoder(t, oa.NewStreamDecoder(strings.NewReader(string(wire))))
	if len(got) != 3 || got[0].Type != ir.Start || got[1].Type != ir.BlockStart || got[2].Type != ir.Error {
		t.Fatalf("events = %+v, want start + block-start + error", got)
	}
	if got[2].Error == nil || got[2].Error.Type != "server_error" || got[2].Error.Message != "boom" {
		t.Errorf("error = %+v", got[2].Error)
	}
}

// Comments (upstream keepalives, H2-2) relay as wire comment lines.
func TestChatStreamEncode_CommentRelay(t *testing.T) {
	evs := []ir.StreamEvent{{Type: ir.Comment}}
	wire := string(encodeChatAll(t, evs))
	if !strings.Contains(wire, ": keepalive\n\n") && !strings.Contains(wire, ": \n\n") &&
		!strings.Contains(wire, ":\n\n") {
		t.Errorf("comment must relay as a wire comment line, got %q", wire)
	}
}
