package anthropic_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	anth "github.com/tubeof/proxyllm/internal/translate/anthropic"
	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/testutil"
)

// Stream-encoder contract tests, RED-GREEN per Analysis F. The encoder must
// produce the Anthropic Messages SSE wire format (facts A5–A7, A13) such
// that the package's own stream decoder reconstructs the canonical events
// (round-trip discipline: the decoder is fixture-verified against
// docs.anthropic.com, so a passing round trip proves wire conformance).

// encodeAll runs evs through a fresh encoder over a buffer and returns the
// wire bytes.
func encodeAll(t *testing.T, evs []ir.StreamEvent) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := anth.NewStreamEncoder(&buf)
	for _, ev := range evs {
		if err := enc.Write(context.Background(), ev); err != nil {
			t.Fatalf("Write(%s): %v", ev.Type, err)
		}
	}
	return buf.Bytes()
}

// fullSequenceEvents is the canonical event sequence matching the golden
// anthropic/stream-events.sse fixture (text + thinking + tool_use, A6
// ordering, signature_delta before the thinking block's content_block_stop).
func fullSequenceEvents() []ir.StreamEvent {
	return []ir.StreamEvent{
		{Type: ir.Start, Response: &ir.Response{
			ID:    "msg_01XFDUDYJgAACzvnptvVoYEL",
			Model: "claude-sonnet-5",
			Usage: ir.Usage{InputTokens: 25, OutputTokens: 1},
		}},
		{Type: ir.Ping},
		{Type: ir.BlockStart, Index: 0, Part: ir.TextPart{}},
		{Type: ir.Delta, Index: 0, Delta: ir.DeltaPayload{Kind: ir.DeltaText, Text: "Hello"}},
		{Type: ir.Delta, Index: 0, Delta: ir.DeltaPayload{Kind: ir.DeltaText, Text: " world"}},
		{Type: ir.BlockStop, Index: 0},
		{Type: ir.BlockStart, Index: 1, Part: ir.ThinkingPart{}},
		{Type: ir.Delta, Index: 1, Delta: ir.DeltaPayload{Kind: ir.DeltaThinking, Text: "Let me think about this."}},
		{Type: ir.Delta, Index: 1, Delta: ir.DeltaPayload{Kind: ir.DeltaSignature, Text: "EqoBCkgIAxgCIkA3MDUxYTYwNTYxYWNiMjQ0YzEyNDkxN2IxMjM0NTY3ODkwYWJjZGVmZ2hpams="}},
		{Type: ir.BlockStop, Index: 1},
		{Type: ir.BlockStart, Index: 2, Part: ir.ToolCallPart{ID: "toolu_01A09q90qw90lq917wa35011", Name: "get_weather"}},
		{Type: ir.Delta, Index: 2, Delta: ir.DeltaPayload{Kind: ir.DeltaToolInput, PartialJSON: `{"location":`}},
		{Type: ir.Delta, Index: 2, Delta: ir.DeltaPayload{Kind: ir.DeltaToolInput, PartialJSON: `"San Francisco"}`}},
		{Type: ir.BlockStop, Index: 2},
		{Type: ir.MessageDelta, MessageDelta: &ir.MessageDeltaPayload{
			StopReason: ir.StopToolUse,
			Usage:      ir.Usage{InputTokens: 25, OutputTokens: 128},
		}},
		{Type: ir.Done},
	}
}

func TestStreamEncode_FullSequenceRoundTrip(t *testing.T) {
	wire := encodeAll(t, fullSequenceEvents())

	// Round trip: the fixture-verified decoder must reconstruct the exact
	// canonical sequence (ping included; comments are lossy at IR level by
	// design — wire bytes stay at the relay layer — and are absent here).
	got := testutil.DrainStreamDecoder(t, anth.NewStreamDecoder(bytes.NewReader(wire)))
	want := fullSequenceEvents()
	testutil.AssertEventsEqual(t, "anthropic/stream-events.sse", want, got)
}

// The wire shape must carry the documented event names and empty-init
// content blocks (A5/A6), not just round-trip semantically: assert the exact
// SSE lines for the start of the stream.
func TestStreamEncode_WireShape(t *testing.T) {
	evs := []ir.StreamEvent{
		{Type: ir.Start, Response: &ir.Response{
			ID:    "msg_01XFDUDYJgAACzvnptvVoYEL",
			Model: "claude-sonnet-5",
			Usage: ir.Usage{InputTokens: 25, OutputTokens: 1},
		}},
		{Type: ir.BlockStart, Index: 0, Part: ir.ToolCallPart{ID: "toolu_01", Name: "get_weather"}},
		{Type: ir.Delta, Index: 0, Delta: ir.DeltaPayload{Kind: ir.DeltaToolInput, PartialJSON: `{"location":`}},
		{Type: ir.BlockStop, Index: 0},
		{Type: ir.Done},
	}
	wire := string(encodeAll(t, evs))

	for _, want := range []string{
		"event: message_start\n",
		`data: {"type":"message_start","message":{"id":"msg_01XFDUDYJgAACzvnptvVoYEL","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":25,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` + "\n\n",
		`event: content_block_start` + "\n",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{}}}` + "\n\n",
		`event: content_block_delta` + "\n",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"location\":"}}` + "\n\n",
		`event: content_block_stop` + "\n",
		`data: {"type":"content_block_stop","index":0}` + "\n\n",
		`event: message_stop` + "\n",
		`data: {"type":"message_stop"}` + "\n\n",
	} {
		if !strings.Contains(wire, want) {
			t.Errorf("wire missing %q\ngot:\n%s", want, wire)
		}
	}
}

// ir.Error maps onto the terminal error SSE event (A13).
func TestStreamEncode_ErrorEvent(t *testing.T) {
	evs := []ir.StreamEvent{
		{Type: ir.Start, Response: &ir.Response{ID: "msg_1", Model: "claude-sonnet-5"}},
		{Type: ir.Error, Error: &ir.UpstreamError{Type: "overloaded_error", Message: "Overloaded"}},
	}
	wire := string(encodeAll(t, evs))
	if !strings.Contains(wire, `event: error`+"\n"+
		`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`+"\n\n") {
		t.Errorf("error event wire = %s", wire)
	}

	// Round trip: the terminal error must come back as ir.Error.
	got := testutil.DrainStreamDecoder(t, anth.NewStreamDecoder(strings.NewReader(wire)))
	if len(got) != 2 || got[1].Type != ir.Error {
		t.Fatalf("events = %+v, want start + error", got)
	}
	if got[1].Error == nil || got[1].Error.Type != "overloaded_error" || got[1].Error.Message != "Overloaded" {
		t.Errorf("error = %+v", got[1].Error)
	}
}

// Comments (upstream keepalives, H2-2) relay as wire comment lines.
func TestStreamEncode_CommentRelay(t *testing.T) {
	evs := []ir.StreamEvent{
		{Type: ir.Comment},
	}
	wire := string(encodeAll(t, evs))
	if !strings.Contains(wire, ": keepalive\n\n") && !strings.Contains(wire, ": \n\n") &&
		!strings.Contains(wire, ":\n\n") {
		t.Errorf("comment must relay as a wire comment line, got %q", wire)
	}
}

// serverToolStreamEvents is the canonical sequence matching the golden
// anthropic/server-tool-stream.sse fixture (server_tool_use with empty
// input + input_json_delta; web_search_tool_result COMPLETE in
// content_block_start; nested server_tool_use usage counters — verified
// live 2026-09-12).
func serverToolStreamEvents() []ir.StreamEvent {
	return []ir.StreamEvent{
		{Type: ir.Start, Response: &ir.Response{
			ID:    "msg_01StreamServerTool",
			Model: "claude-sonnet-5",
			Usage: ir.Usage{InputTokens: 25, OutputTokens: 1},
		}},
		{Type: ir.BlockStart, Index: 0, Part: ir.TextPart{}},
		{Type: ir.Delta, Index: 0, Delta: ir.DeltaPayload{Kind: ir.DeltaText, Text: "Searching for current weather."}},
		{Type: ir.BlockStop, Index: 0},
		{Type: ir.BlockStart, Index: 1, Part: ir.ServerToolUsePart{ID: "srvtoolu_014hJH82Qum7Td6UV8gDXThB", Name: "web_search"}},
		{Type: ir.Delta, Index: 1, Delta: ir.DeltaPayload{Kind: ir.DeltaToolInput, PartialJSON: `{"query":"latest quantum computing breakthroughs 2025"}`}},
		{Type: ir.BlockStop, Index: 1},
		{Type: ir.BlockStart, Index: 2, Part: ir.ServerToolResultPart{
			ToolUseID: "srvtoolu_014hJH82Qum7Td6UV8gDXThB",
			ToolName:  "web_search",
			Search: []ir.WebSearchResultItem{{
				URL:              "https://example.com",
				Title:            "Quantum Computing Breakthroughs in 2025",
				EncryptedContent: "Ev0DCioIAxgCIiQ3NmU4ZmI4OC1k...",
			}},
		}},
		{Type: ir.BlockStop, Index: 2},
		{Type: ir.BlockStart, Index: 3, Part: ir.TextPart{}},
		{Type: ir.Delta, Index: 3, Delta: ir.DeltaPayload{Kind: ir.DeltaText, Text: "Based on the search, recent breakthroughs include..."}},
		{Type: ir.BlockStop, Index: 3},
		{Type: ir.MessageDelta, MessageDelta: &ir.MessageDeltaPayload{
			StopReason: ir.StopEndTurn,
			Usage:      ir.Usage{InputTokens: 10682, OutputTokens: 510, WebSearchRequests: 1},
		}},
		{Type: ir.Done},
	}
}

func TestStreamEncode_ServerToolBlocksRoundTrip(t *testing.T) {
	wire := encodeAll(t, serverToolStreamEvents())
	got := testutil.DrainStreamDecoder(t, anth.NewStreamDecoder(bytes.NewReader(wire)))
	testutil.AssertEventsEqual(t, "anthropic/server-tool-stream.sse", serverToolStreamEvents(), got)
}

// The server_tool_use content_block_start must carry "input":{} explicitly
// (streaming wire shape, verified live 2026-09-12) — never omitted — and
// the web_search_tool_result must encode COMPLETE in content_block_start
// with the array content asymmetry, plus nested usage counters.
func TestStreamEncode_ServerToolWireShape(t *testing.T) {
	wire := string(encodeAll(t, serverToolStreamEvents()))

	for _, want := range []string{
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"server_tool_use","id":"srvtoolu_014hJH82Qum7Td6UV8gDXThB","name":"web_search","input":{}}}` + "\n\n",
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"latest quantum computing breakthroughs 2025\"}"}}` + "\n\n",
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_014hJH82Qum7Td6UV8gDXThB","content":[{"type":"web_search_result","url":"https://example.com","title":"Quantum Computing Breakthroughs in 2025","encrypted_content":"Ev0DCioIAxgCIiQ3NmU4ZmI4OC1k..."}]}}` + "\n\n",
		`"usage":{"input_tokens":10682,"output_tokens":510,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"server_tool_use":{"web_search_requests":1}}}` + "\n\n",
	} {
		if !strings.Contains(wire, want) {
			t.Errorf("wire missing %q\ngot:\n%s", want, wire)
		}
	}
}
