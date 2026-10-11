package routing_test

// Unsolicited thinking blocks (reported 2026-10-11): "Web Fetch weiterhin
// verbuggt … keine Antwort oder 404". The reference returns thinking blocks
// only for requests that ENABLED thinking; a client that asked for none gets
// plain content. Claude Code's helper calls (the web_fetch summarizer sends
// no thinking parameter at all) read the FIRST content block as text, so an
// unsolicited thinking block reads as an empty answer — every WebFetch
// ended as "No response from model".
//
// The contract these tests pin:
//
//   - ApplyCompat records on the request whether the CLIENT enabled
//     thinking, before the policy rewrites the parameter (a stripped
//     parameter must not erase the client's intent).
//   - A response (streamed or complete) to a request that never enabled
//     thinking carries no thinking block, and the drop is traced.
//   - A request that DID enable thinking keeps its thinking blocks.

import (
	"context"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// thinkingProvider scripts a target that answers with a thinking block
// followed by text, on both paths — the shape glm-5.3 returns for a request
// that asked for no thinking at all.
type thinkingProvider struct {
	completeCalls int
	streamCalls   int
}

func (p *thinkingProvider) ID() string { return "think" }
func (p *thinkingProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{Tools: true, Thinking: true}
}
func (p *thinkingProvider) RefreshCredentials(_ context.Context) error { return nil }

func (p *thinkingProvider) Complete(_ context.Context, _ ir.Request) (ir.Response, error) {
	p.completeCalls++
	return ir.Response{
		ID:         "m1",
		StopReason: ir.StopEndTurn,
		Content: []ir.Part{
			ir.ThinkingPart{Thinking: "we need answer the user"},
			ir.TextPart{Text: "the answer"},
		},
	}, nil
}

func (p *thinkingProvider) Stream(_ context.Context, _ ir.Request) (<-chan ir.StreamEvent, func(), error) {
	p.streamCalls++
	ch := make(chan ir.StreamEvent, 16)
	ch <- ir.StreamEvent{Type: ir.Start}
	ch <- ir.StreamEvent{Type: ir.BlockStart, Index: 0, Part: ir.ThinkingPart{}}
	ch <- ir.StreamEvent{Type: ir.Delta, Index: 0, Delta: ir.DeltaPayload{Kind: ir.DeltaThinking, Text: "we need answer"}}
	ch <- ir.StreamEvent{Type: ir.BlockStop, Index: 0}
	ch <- ir.StreamEvent{Type: ir.BlockStart, Index: 1, Part: ir.TextPart{}}
	ch <- ir.StreamEvent{Type: ir.Delta, Index: 1, Delta: ir.DeltaPayload{Kind: ir.DeltaText, Text: "the answer"}}
	ch <- ir.StreamEvent{Type: ir.BlockStop, Index: 1}
	ch <- ir.StreamEvent{Type: ir.MessageDelta, MessageDelta: &ir.MessageDeltaPayload{StopReason: ir.StopEndTurn}}
	ch <- ir.StreamEvent{Type: ir.Done}
	close(ch)
	return ch, func() {}, nil
}

func thinkingExecutor(p provider.Provider) *routing.Executor {
	return routing.NewExecutor(routing.ExecutorConfig{
		Registry: regOf(p),
		Health:   routing.NewHealthTracker(),
	})
}

func plainReq() ir.Request {
	return ir.Request{
		Model: "m",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "summarize the page"}}},
		},
	}
}

func drainStream(t *testing.T, ch <-chan ir.StreamEvent) []ir.StreamEvent {
	t.Helper()
	var events []ir.StreamEvent
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return events
			}
			events = append(events, ev)
		case <-time.After(2 * time.Second):
			t.Fatal("stream did not drain in time")
		}
	}
}

// --- the recorded intent -------------------------------------------------

func TestApplyCompat_RecordsWhetherTheClientEnabledThinking(t *testing.T) {
	cases := []struct {
		name string
		cfg  *ir.ThinkingConfig
		want bool
	}{
		{"no thinking parameter", nil, false},
		{"explicitly disabled", &ir.ThinkingConfig{}, false},
		{"enabled with a budget", &ir.ThinkingConfig{Enabled: true, BudgetTokens: 10000}, true},
		{"adaptive", &ir.ThinkingConfig{Adaptive: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := plainReq()
			req.Thinking = tc.cfg
			// map-to-reasoning-effort rewrites the parameter (and nils it on
			// an enabled request) — the recorded intent must survive that.
			got, _, err := routing.ApplyCompat(routing.Compat{ThinkingHandling: routing.ThinkingMapToEffort}, req)
			if err != nil {
				t.Fatalf("ApplyCompat: %v", err)
			}
			if got.ThinkingRequested != tc.want {
				t.Errorf("ThinkingRequested = %v, want %v", got.ThinkingRequested, tc.want)
			}
		})
	}
}

// --- the complete path ---------------------------------------------------

func TestExecute_UnsolicitedThinkingBlockIsDropped(t *testing.T) {
	p := &thinkingProvider{}
	e := thinkingExecutor(p)

	resp, trace, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "think"}}, plainReq())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("content = %+v, want just the answer text", resp.Content)
	}
	if tp, ok := resp.Content[0].(ir.TextPart); !ok || tp.Text != "the answer" {
		t.Errorf("content[0] = %#v, want the answer text (a client reads content[0] as text)", resp.Content[0])
	}
	if !hasCompatPolicy(trace, "thinking_handling") {
		t.Error("dropping an unsolicited thinking block must be traced (never silent)")
	}
}

func TestExecute_RequestedThinkingIsKept(t *testing.T) {
	p := &thinkingProvider{}
	e := thinkingExecutor(p)

	req := plainReq()
	req.ThinkingRequested = true
	resp, _, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "think"}}, req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("content = %+v, want thinking + text (the client asked for thinking)", resp.Content)
	}
	if _, ok := resp.Content[0].(ir.ThinkingPart); !ok {
		t.Errorf("content[0] = %#v, want the thinking block the client requested", resp.Content[0])
	}
}

// An executor called directly (no ApplyCompat in front) still sees the
// client's parameter on the request: an enabled thinking param keeps the
// blocks even without the recorded verdict.
func TestExecute_PreCompatThinkingParamKeepsBlocks(t *testing.T) {
	p := &thinkingProvider{}
	e := thinkingExecutor(p)

	req := plainReq()
	req.Thinking = &ir.ThinkingConfig{Enabled: true, BudgetTokens: 4096}
	resp, _, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "think"}}, req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("content = %+v, want thinking + text", resp.Content)
	}
}

// --- the streaming path --------------------------------------------------

// The reported failure rode the STREAM: the summarizer call is streamed, and
// Claude Code assembles the message from the events.
func TestExecuteStream_UnsolicitedThinkingBlockIsDropped(t *testing.T) {
	p := &thinkingProvider{}
	e := thinkingExecutor(p)

	ch, cancel, trace, err := e.ExecuteStream(context.Background(),
		[]routing.ChainEntry{{ProviderID: "think"}}, plainReq())
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	defer cancel()

	events := drainStream(t, ch)
	var blocks []ir.Part
	for _, ev := range events {
		if ev.Type == ir.BlockStart {
			if _, bad := ev.Part.(ir.ThinkingPart); bad {
				t.Errorf("client-facing stream carries an unsolicited thinking block: %+v", ev)
			}
			blocks = append(blocks, ev.Part)
		}
		if ev.Type == ir.Delta && ev.Delta.Kind == ir.DeltaThinking {
			t.Errorf("client-facing stream carries a thinking delta: %+v", ev)
		}
	}
	// Indexes must stay contiguous from 0: the encoder relays ev.Index
	// verbatim, and a hole is not a shape the reference emits.
	wantIndex := 0
	for _, ev := range events {
		if ev.Type != ir.BlockStart {
			continue
		}
		if ev.Index != wantIndex {
			t.Errorf("block index = %d, want %d (contiguous after the drop)", ev.Index, wantIndex)
		}
		wantIndex++
	}
	if len(blocks) != 1 {
		t.Fatalf("blocks = %+v, want just the answer text", blocks)
	}
	if _, ok := blocks[0].(ir.TextPart); !ok {
		t.Errorf("block = %#v, want the answer text", blocks[0])
	}
	if !hasCompatPolicy(trace, "thinking_handling") {
		t.Error("dropping an unsolicited thinking block must be traced (never silent)")
	}
}

func TestExecuteStream_RequestedThinkingIsKept(t *testing.T) {
	p := &thinkingProvider{}
	e := thinkingExecutor(p)

	req := plainReq()
	req.ThinkingRequested = true
	ch, cancel, _, err := e.ExecuteStream(context.Background(),
		[]routing.ChainEntry{{ProviderID: "think"}}, req)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	defer cancel()

	events := drainStream(t, ch)
	var sawThinking bool
	for _, ev := range events {
		if ev.Type == ir.BlockStart {
			if _, ok := ev.Part.(ir.ThinkingPart); ok {
				sawThinking = true
			}
		}
	}
	if !sawThinking {
		t.Error("a thinking block the client asked for must reach the client")
	}
}

// hasCompatPolicy reports whether the trace records a compat policy
// application for the given setting.
func hasCompatPolicy(trace *routing.Trace, setting string) bool {
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventCompatPolicy && ev.From == setting {
			return true
		}
	}
	return false
}
