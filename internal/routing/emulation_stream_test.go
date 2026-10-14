package routing_test

// Streaming-path emulation: ExecuteStream used to relay raw tool_use events
// for emulated web tools with no proxy-side answers (the emulation round-trip
// loop only ran on the complete path), which left a streaming client with a
// tool call nothing would ever answer. The contract: a streaming request whose
// tools include emulated defs runs the SAME complete-path emulation round-trip
// to finality and synthesizes the protocol-conform event stream from the final
// response. Latency trade:
// no token streaming for those requests (documented in routing-and-
// fallbacks.md / ADR-010 Amendment).

import (
	"context"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// TestExecuteStream_StreamEmulatesWebTools: streaming + emulated defs →
// server_tool_use + web_search_tool_result blocks synthesized into the
// event stream, then the final answer; honest usage; trace records it.
func TestExecuteStream_StreamEmulatesWebTools(t *testing.T) {
	p := &emulProvider{}
	emu := &fakeEmulator{searchAnswers: []string{`[
		{"URL":"https://go.dev/dl/","Title":"Go Downloads","EncryptedContent":"","PageAge":""}
	]`}}
	e := roundTripExecutor(p, emu)

	ch, cancel, trace, err := e.ExecuteStream(context.Background(),
		[]routing.ChainEntry{{ProviderID: "emul"}}, emulatedSearchRequest())
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	defer cancel()

	var events []ir.StreamEvent
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				goto drained
			}
			events = append(events, ev)
		case <-time.After(2 * time.Second):
			t.Fatal("stream did not drain in time")
		}
	}
drained:
	// The synthesized events walk the content: ServerToolUsePart +
	// ServerToolResultPart must both appear as BlockStarts; the answer
	// text as a text delta; the stream ends with done.
	var sawUse, sawResult, sawText, sawDone bool
	for _, ev := range events {
		switch ev.Type {
		case ir.BlockStart:
			if u, ok := ev.Part.(ir.ServerToolUsePart); ok && u.Name == "web_search" {
				sawUse = true
			}
			if r, ok := ev.Part.(ir.ServerToolResultPart); ok && r.ToolName == "web_search" && !r.IsError {
				sawResult = true
			}
		case ir.Delta:
			if ev.Delta.Kind == ir.DeltaText && ev.Delta.Text == "done" {
				sawText = true
			}
		case ir.Done:
			sawDone = true
		}
	}
	if !sawUse || !sawResult || !sawText || !sawDone {
		t.Fatalf("stream events incomplete: sawUse=%v sawResult=%v sawText=%v sawDone=%v\n%+v",
			sawUse, sawResult, sawText, sawDone, events)
	}

	// The provider saw the round trip (2 Complete calls), never Stream.
	if p.calls != 2 {
		t.Fatalf("provider calls = %d, want 2 (tool round trip on the complete path)", p.calls)
	}
	if p.streamCalls != 0 {
		t.Fatalf("Stream calls = %d, want 0 (emulated request rides the complete loop)", p.streamCalls)
	}

	// Traced — never silent.
	found := false
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventEmulatedTool {
			found = true
		}
	}
	if !found {
		t.Error("trace must record the emulated-tool substitution on the stream path")
	}
}

// The detour must dispatch stream:false — a complete
// request carrying true makes real upstreams answer chunked SSE, which
// the non-streaming decode cannot parse (the whole request class would
// fail instead of being emulated). Pinned via the provider's view.
func TestExecuteStream_StreamEmulatesWithStreamFalse(t *testing.T) {
	p := &emulProvider{}
	emu := &fakeEmulator{searchAnswers: []string{`[]`}}
	e := roundTripExecutor(p, emu)

	ch, cancel, _, err := e.ExecuteStream(context.Background(),
		[]routing.ChainEntry{{ProviderID: "emul"}},
		func(r ir.Request) ir.Request { r.Stream = true; return r }(emulatedSearchRequest()))
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	defer cancel()
	for ev := range ch {
		_ = ev // drain
	}
	if p.lastReq.Stream {
		t.Fatal("the emulated round trip must dispatch with stream:false")
	}
}

// A cancellation arriving while the emulated round trip is still running
// must not deadlock: the stream closes (with at most an error event) and
// ExecuteStream's returned cancel releases everything.
func TestExecuteStream_StreamEmulationRespectsCancel(t *testing.T) {
	p := &emulProvider{}
	emu := &fakeEmulator{searchAnswers: []string{`[]`}}
	e := roundTripExecutor(p, emu)

	ctx, c := context.WithCancel(context.Background())
	ch, cancel, _, err := e.ExecuteStream(ctx,
		[]routing.ChainEntry{{ProviderID: "emul"}}, emulatedSearchRequest())
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	defer cancel()
	c()

	// The channel must close promptly; any events before that are legal
	// (already-dispatched rounds may have produced some).
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // closed — no deadlock, no leak
			}
		case <-deadline:
			t.Fatal("emulated stream did not close after cancellation")
		}
	}
}
