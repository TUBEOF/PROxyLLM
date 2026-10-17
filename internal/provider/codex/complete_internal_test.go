package codex

// assembleResponse unit tests. The event streams are hand-built: the wire
// shapes the decoder produces are pinned by the openai/translate contract
// tests; these cases cover the states an HTTP handler cannot reach
// deterministically (duplicate stops, missing stops, unknown indices,
// terminal errors, cancellation).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// feed closes the channel, so assembleResponse always sees a terminal close
// (the pump does the same after a terminal event or a cancellation).
func feed(events ...ir.StreamEvent) <-chan ir.StreamEvent {
	ch := make(chan ir.StreamEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch
}

// The decoder can emit a BlockStop for the previous block when an item type
// it does not open a block for ends (an unrecognized output_item). A second
// stop must not duplicate the part.
func TestAssembleResponse_DuplicateBlockStopKeepsOnePart(t *testing.T) {
	got, err := assembleResponse(context.Background(), feed(
		ir.StreamEvent{Type: ir.BlockStart, Index: 0, Part: ir.TextPart{}},
		ir.StreamEvent{Type: ir.Delta, Index: 0, Delta: ir.DeltaPayload{Kind: ir.DeltaText, Text: "a"}},
		ir.StreamEvent{Type: ir.BlockStop, Index: 0},
		ir.StreamEvent{Type: ir.BlockStop, Index: 0},
		ir.StreamEvent{Type: ir.Done},
	))
	if err != nil {
		t.Fatalf("assembleResponse: %v", err)
	}
	if len(got.Content) != 1 {
		t.Fatalf("content = %+v, want exactly one part", got.Content)
	}
}

// A stream that ends after the terminal MessageDelta without individual
// BlockStops still yields its full content.
func TestAssembleResponse_MissingBlockStopStillFinalizes(t *testing.T) {
	got, err := assembleResponse(context.Background(), feed(
		ir.StreamEvent{Type: ir.BlockStart, Index: 0, Part: ir.TextPart{}},
		ir.StreamEvent{Type: ir.Delta, Index: 0, Delta: ir.DeltaPayload{Kind: ir.DeltaText, Text: "hello"}},
		ir.StreamEvent{Type: ir.MessageDelta, MessageDelta: &ir.MessageDeltaPayload{StopReason: ir.StopEndTurn}},
		ir.StreamEvent{Type: ir.Done},
	))
	if err != nil {
		t.Fatalf("assembleResponse: %v", err)
	}
	if len(got.Content) != 1 {
		t.Fatalf("content = %+v, want the open block finalized", got.Content)
	}
	if tp, ok := got.Content[0].(ir.TextPart); !ok || tp.Text != "hello" {
		t.Errorf("content[0] = %+v, want the accumulated text", got.Content[0])
	}
}

// A tool call whose arguments never arrived carries an empty object, never
// an empty fragment — the house convention for a parameterless call.
func TestAssembleResponse_EmptyToolInputIsAnEmptyObject(t *testing.T) {
	got, err := assembleResponse(context.Background(), feed(
		ir.StreamEvent{Type: ir.BlockStart, Index: 0, Part: ir.ToolCallPart{ID: "call_1", Name: "get_time"}},
		ir.StreamEvent{Type: ir.BlockStop, Index: 0},
		ir.StreamEvent{Type: ir.Done},
	))
	if err != nil {
		t.Fatalf("assembleResponse: %v", err)
	}
	if len(got.Content) != 1 {
		t.Fatalf("content = %+v, want one tool call", got.Content)
	}
	tc, ok := got.Content[0].(ir.ToolCallPart)
	if !ok {
		t.Fatalf("content[0] = %T, want ir.ToolCallPart", got.Content[0])
	}
	if string(tc.Input) != "{}" {
		t.Errorf("input = %s, want {}", tc.Input)
	}
}

// Signature fragments accumulate onto the thinking block; no signature is
// ever fabricated, so an empty stream leaves it empty.
func TestAssembleResponse_SignatureDeltaAccumulates(t *testing.T) {
	got, err := assembleResponse(context.Background(), feed(
		ir.StreamEvent{Type: ir.BlockStart, Index: 0, Part: ir.ThinkingPart{}},
		ir.StreamEvent{Type: ir.Delta, Index: 0, Delta: ir.DeltaPayload{Kind: ir.DeltaThinking, Text: "pondering"}},
		ir.StreamEvent{Type: ir.Delta, Index: 0, Delta: ir.DeltaPayload{Kind: ir.DeltaSignature, Text: "sig"}},
		ir.StreamEvent{Type: ir.BlockStop, Index: 0},
		ir.StreamEvent{Type: ir.Done},
	))
	if err != nil {
		t.Fatalf("assembleResponse: %v", err)
	}
	if len(got.Content) != 1 {
		t.Fatalf("content = %+v, want one thinking part", got.Content)
	}
	tp, ok := got.Content[0].(ir.ThinkingPart)
	if !ok {
		t.Fatalf("content[0] = %T, want ir.ThinkingPart", got.Content[0])
	}
	if tp.Thinking != "pondering" || tp.Signature != "sig" {
		t.Errorf("thinking part = %+v, want the accumulated text and signature", tp)
	}
}

// Deltas for an index with no open block are ignored, never a panic.
func TestAssembleResponse_DeltaForUnopenedIndexIsIgnored(t *testing.T) {
	got, err := assembleResponse(context.Background(), feed(
		ir.StreamEvent{Type: ir.Delta, Index: 3, Delta: ir.DeltaPayload{Kind: ir.DeltaText, Text: "stray"}},
		ir.StreamEvent{Type: ir.Done},
	))
	if err != nil {
		t.Fatalf("assembleResponse: %v", err)
	}
	if len(got.Content) != 0 {
		t.Errorf("content = %+v, want none", got.Content)
	}
}

// A terminal canonical error becomes the typed upstream error, status 0:
// nothing reached the client, so routing may still fail over.
func TestAssembleResponse_TerminalErrorBecomesUpstreamError(t *testing.T) {
	_, err := assembleResponse(context.Background(), feed(
		ir.StreamEvent{Type: ir.Error, Error: &ir.UpstreamError{
			Type: "server_error", Code: "boom", Message: "upstream fell over",
		}},
	))
	var up *provider.UpstreamError
	if !errors.As(err, &up) {
		t.Fatalf("error = %v (%T), want *provider.UpstreamError", err, err)
	}
	if up.HTTPStatus != 0 || up.Type != "server_error" || up.Code != "boom" || up.Message != "upstream fell over" {
		t.Errorf("upstream error = %+v, want the terminal event's fields with status 0", up)
	}
}

// A channel that closes without Done or Error is an error, never a silent
// partial response.
func TestAssembleResponse_NoTerminalEventIsAnError(t *testing.T) {
	_, err := assembleResponse(context.Background(), feed(
		ir.StreamEvent{Type: ir.Start, Response: &ir.Response{ID: "resp_x"}},
	))
	if err == nil {
		t.Fatal("a stream without a terminal event must fail")
	}
	if !strings.Contains(err.Error(), "terminal event") {
		t.Errorf("error = %q, want it to name the missing terminal event", err)
	}
}

// A canceled caller is reported as context.Canceled and NOT as an upstream
// error: routing must classify it as a client abort, never as evidence
// about the account.
func TestAssembleResponse_CanceledContextReturnsContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := assembleResponse(ctx, feed(
		ir.StreamEvent{Type: ir.Start, Response: &ir.Response{ID: "resp_x"}},
	))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	var up *provider.UpstreamError
	if errors.As(err, &up) {
		t.Error("a canceled caller must not read as an upstream error")
	}
}
