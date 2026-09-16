package ollama_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/translate/ir"
	ol "github.com/tubeof/proxyllm/internal/translate/ollama"
	"github.com/tubeof/proxyllm/internal/translate/testutil"
)

// Stream-encoder contract tests, RED-GREEN per Analysis F. The encoder must
// produce the Ollama /api/chat NDJSON stream wire format (pinned via the
// golden fixture ollama/chat-stream.ndjson, OP-4; facts L10-L11) such that
// the package's own stream decoder reconstructs the canonical events
// (round-trip discipline: the decoder is fixture-verified, so a passing
// round trip proves wire conformance).
//
// created_at is a runtime timestamp, so line-shape assertions pin everything
// around it via suffixes from `,"message":` on, which also proves the
// fixture's field order model → created_at → message → done → done_reason →
// counts.

// encodeOllamaAll runs evs through a fresh ollama encoder over a buffer and
// returns the wire bytes.
func encodeOllamaAll(t *testing.T, evs []ir.StreamEvent) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := ol.NewStreamEncoder(&buf)
	for _, ev := range evs {
		if err := enc.Write(context.Background(), ev); err != nil {
			t.Fatalf("Write(%s): %v", ev.Type, err)
		}
	}
	return buf.Bytes()
}

// lines splits encoded NDJSON wire into its lines.
func lines(t *testing.T, wire []byte) []string {
	t.Helper()
	s := strings.TrimSpace(string(wire))
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// The fixture-matching canonical sequence (text + thinking) must round trip
// through the fixture-verified decoder event-for-event — this is the same
// sequence the golden .expected.json pins, so it doubles as the fixture's
// canonical form.
func TestOllamaStreamEncode_FullSequenceRoundTrip(t *testing.T) {
	rel := "ollama/chat-stream.ndjson.expected.json"
	want := testutil.LoadExpectedEvents(t, rel)

	wire := encodeOllamaAll(t, want)
	got := testutil.DrainStreamDecoder(t, ol.NewStreamDecoder(bytes.NewReader(wire)))
	testutil.AssertEventsEqual(t, rel, want, got)

	// Wire economy: exactly the fixture's five lines (framing, two content
	// fragments, one thinking fragment, one done line).
	if n := len(lines(t, wire)); n != 5 {
		t.Errorf("wire = %d lines, want 5 (fixture chat-stream.ndjson):\n%s", n, wire)
	}
}

// Tool calls ride message.tool_calls with OBJECT arguments delivered as one
// complete JSON value per entry (L10) — the wire cannot carry fragment
// boundaries or tool IDs (lossy, like the non-stream wire), so the canonical
// input uses a single complete fragment and no ID.
func TestOllamaStreamEncode_ToolCallRoundTrip(t *testing.T) {
	evs := []ir.StreamEvent{
		{Type: ir.Start, Response: &ir.Response{Model: "llama3.3"}},
		{Type: ir.BlockStart, Index: 0, Part: ir.TextPart{}},
		{Type: ir.Delta, Index: 0, Delta: ir.DeltaPayload{Kind: ir.DeltaText, Text: "Hi"}},
		{Type: ir.BlockStop, Index: 0},
		{Type: ir.BlockStart, Index: 1, Part: ir.ToolCallPart{Name: "get_weather"}},
		{Type: ir.Delta, Index: 1, Delta: ir.DeltaPayload{Kind: ir.DeltaToolInput, PartialJSON: `{"location":"San Francisco"}`}},
		{Type: ir.BlockStop, Index: 1},
		{Type: ir.MessageDelta, MessageDelta: &ir.MessageDeltaPayload{
			StopReason: ir.StopToolUse,
			Usage:      ir.Usage{InputTokens: 26, OutputTokens: 10},
		}},
		{Type: ir.Done},
	}
	wire := encodeOllamaAll(t, evs)

	got := testutil.DrainStreamDecoder(t, ol.NewStreamDecoder(bytes.NewReader(wire)))
	testutil.AssertEventsEqual(t, "ollama/tool-call round trip", evs, got)
}

// The wire shape must match the golden fixture's line shapes exactly (after
// the runtime created_at), not just round-trip semantically.
func TestOllamaStreamEncode_WireShape(t *testing.T) {
	rel := "ollama/chat-stream.ndjson.expected.json"
	wire := encodeOllamaAll(t, testutil.LoadExpectedEvents(t, rel))
	ls := lines(t, wire)

	// Framing line (fixture line 1): empty content announces the message.
	if !strings.HasPrefix(ls[0], `{"model":"llama3.3","created_at":"`) {
		t.Errorf("line 1 must start model → created_at, got %q", ls[0])
	}
	// Content lines (fixture lines 2-3) carry only the content fragment.
	if !strings.HasSuffix(ls[1], `","message":{"role":"assistant","content":"Hello"},"done":false}`) {
		t.Errorf("content line = %q", ls[1])
	}
	// Thinking lines (fixture line 4) carry thinking with content present.
	if !strings.HasSuffix(ls[3], `","message":{"role":"assistant","content":"","thinking":"Let me think."},"done":false}`) {
		t.Errorf("thinking line = %q", ls[3])
	}
	// Done line (fixture line 5): done_reason + usage counts in fixture
	// field order; durations are provider telemetry with no IR channel.
	last := ls[len(ls)-1]
	if !strings.HasSuffix(last, `","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":26,"prompt_eval_cached_count":8,"eval_count":283}`) {
		t.Errorf("done line = %q", last)
	}
	if strings.Contains(last, "total_duration") {
		t.Errorf("done line must not carry telemetry durations (no IR channel), got %q", last)
	}
}

// Tool arguments marshal as an embedded JSON OBJECT, not a string (L10), and
// argument fragments accumulate into the one complete value the wire format
// requires — fragments never reach the wire split.
func TestOllamaStreamEncode_ToolArgumentsWireShape(t *testing.T) {
	evs := []ir.StreamEvent{
		{Type: ir.Start, Response: &ir.Response{Model: "llama3.3"}},
		{Type: ir.BlockStart, Index: 0, Part: ir.ToolCallPart{Name: "get_weather"}},
		{Type: ir.Delta, Index: 0, Delta: ir.DeltaPayload{Kind: ir.DeltaToolInput, PartialJSON: `{"location":`}},
		{Type: ir.Delta, Index: 0, Delta: ir.DeltaPayload{Kind: ir.DeltaToolInput, PartialJSON: `"San Francisco"}`}},
		{Type: ir.BlockStop, Index: 0},
		{Type: ir.Done},
	}
	wire := encodeOllamaAll(t, evs)

	// One tool line with the assembled object arguments.
	if !strings.Contains(string(wire), `"tool_calls":[{"function":{"name":"get_weather","arguments":{"location":"San Francisco"}}}]`) {
		t.Errorf("tool line must carry object arguments assembled from fragments, got:\n%s", wire)
	}
}

// Done without a preceding MessageDelta still terminates the stream with a
// done:true line — the wire contract requires the final done line (fixture
// line 5), and a downstream decoder ends cleanly instead of at EOF.
func TestOllamaStreamEncode_DoneWithoutMessageDelta(t *testing.T) {
	evs := []ir.StreamEvent{
		{Type: ir.Start, Response: &ir.Response{Model: "llama3.3"}},
		{Type: ir.BlockStart, Index: 0, Part: ir.TextPart{}},
		{Type: ir.Delta, Index: 0, Delta: ir.DeltaPayload{Kind: ir.DeltaText, Text: "Hi"}},
		{Type: ir.BlockStop, Index: 0},
		{Type: ir.Done},
	}
	wire := encodeOllamaAll(t, evs)
	ls := lines(t, wire)

	if n := len(ls); n != 3 {
		t.Errorf("wire = %d lines, want 3 (framing, content, done), got:\n%s", n, wire)
	}
	// Spec-misreading fix (RED-GREEN discipline): the golden fixture's done
	// line (chat-stream.ndjson line 5) places done_reason AFTER done, and the
	// done line carries done_reason even without a preceding MessageDelta —
	// the encoder defaults it via stopReasonToWire(StopEndTurn)="stop".
	if !strings.HasSuffix(ls[len(ls)-1], `","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop"}`) {
		t.Errorf("terminal line must carry done:true + done_reason, got %q", ls[len(ls)-1])
	}

	// Round trip: the decoder synthesizes the terminal MessageDelta from the
	// done line (end_turn, zero usage) and ends at Done.
	got := testutil.DrainStreamDecoder(t, ol.NewStreamDecoder(bytes.NewReader(wire)))
	if len(got) != 6 {
		t.Fatalf("events = %+v, want 6 (start, block_start, delta, block_stop, message_delta, done)", got)
	}
	if got[4].Type != ir.MessageDelta || got[4].MessageDelta.StopReason != ir.StopEndTurn {
		t.Errorf("synthesized message_delta = %+v, want end_turn", got[4].MessageDelta)
	}
}
