package routing_test

// Emulation time budgets (2026-10-10).
//
// Spec: the operator report that a web tool sometimes takes "unfassbar lange".
// Two unbounded waits were possible, both inline on the request path:
//
//   - the ACCOUNT WALK — the pool is one Emulator that walks its account list
//     internally, so N accounts meant N × the client timeout for a single tool
//     call (ExecutorConfig.EmulationCallBudget),
//   - the EMULATION LOOP — a model that keeps issuing tool calls was bounded in
//     COUNT (maxEmulationRounds) but not in time (EmulationTotalBudget).
//
// A stalled account or a busy model must end with an answer the client can act
// on, within a bound the operator set.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// stalledEmulator models an account that accepts the call and never answers — a
// hung upstream or a black-holed connection. It returns only when its context
// ends, which is exactly what a budget must supply.
type stalledEmulator struct{ calls int }

func (s *stalledEmulator) Search(ctx context.Context, _ string, _ ir.ServerToolConfig) ([]ir.WebSearchResultItem, error) {
	s.calls++
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *stalledEmulator) Fetch(ctx context.Context, _ string, _ ir.ServerToolConfig) (*ir.WebFetchContent, error) {
	s.calls++
	<-ctx.Done()
	return nil, ctx.Err()
}

// One invocation, one budget — whatever the account count. Before this, each
// account added its own client timeout to a single tool call's cost.
func TestExecute_StalledEmulatedCallIsBoundedByItsBudget(t *testing.T) {
	p := &emulProvider{}
	emu := &stalledEmulator{}
	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:            regOf(p),
		Health:              routing.NewHealthTracker(),
		Emulator:            emu,
		EmulationCallBudget: 60 * time.Millisecond,
	})

	start := time.Now()
	resp, trace, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "emul"}}, emulatedSearchRequest())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Execute: %v — a stalled tool call is answered with an error RESULT, never a failed request", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("elapsed %v — the call budget did not bound the stalled account", elapsed)
	}
	if emu.calls == 0 {
		t.Fatal("the emulator was never called")
	}

	// The model is told the tool call failed, in the documented shape.
	var errorResult bool
	for _, part := range resp.Content {
		if r, ok := part.(ir.ServerToolResultPart); ok && r.ToolName == "web_search" && r.IsError {
			errorResult = true
		}
	}
	if !errorResult {
		t.Errorf("content = %+v, want a protocol-conform error result for the stalled call", resp.Content)
	}

	// And the log names OUR budget: a bare "context deadline exceeded" reads
	// like an upstream failure when in fact this proxy stopped waiting.
	var noted bool
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventEmulatedTool && strings.Contains(ev.Reason, "budget") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("trace = %+v, want the exhausted budget named in the emulated-tool event", trace.Events())
	}
}

// slowEmulator answers after a delay: the shape a real account has when the
// loop is slow enough for its time budget to be the binding constraint (a fast
// loop is capped by maxEmulationRounds long before any time budget matters).
type slowEmulator struct {
	delay time.Duration
	calls int
}

func (s *slowEmulator) Search(_ context.Context, _ string, _ ir.ServerToolConfig) ([]ir.WebSearchResultItem, error) {
	s.calls++
	time.Sleep(s.delay)
	return []ir.WebSearchResultItem{{URL: "https://go.dev/", Title: "Go"}}, nil
}

func (s *slowEmulator) Fetch(_ context.Context, _ string, _ ir.ServerToolConfig) (*ir.WebFetchContent, error) {
	s.calls++
	time.Sleep(s.delay)
	return &ir.WebFetchContent{URL: "https://go.dev/", Data: "page"}, nil
}

// The loop's own budget: with slow tool calls the model can no longer run to the
// round cap (32 × the call latency) — the request ends at the deadline with what
// the tools answered so far.
func TestExecute_EmulationLoopIsBoundedByItsTotalBudget(t *testing.T) {
	p := &loopingProvider{name: "web_search", answer: `{"query":"again"}`, max: 1000}
	emu := &slowEmulator{delay: 10 * time.Millisecond}
	const budget = 60 * time.Millisecond
	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:             regOf(p),
		Health:               routing.NewHealthTracker(),
		Emulator:             emu,
		EmulationTotalBudget: budget,
	})

	// Unlimited invocations per tool, so every round really reaches the emulator
	// (the shared request's max_uses of 5 would make later rounds fail instantly
	// and let the round cap win the race against the time budget).
	req := emulatedSearchRequest()
	req.Tools[0].ServerTool = &ir.ServerToolConfig{MaxUses: 1000}

	start := time.Now()
	resp, trace, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "loop"}}, req)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Execute: %v (provider calls %d) — the loop ends with what it has, not with an error", err, p.calls)
	}
	// 32 rounds of 10 ms would be >300 ms; the budget has to cut that short.
	if elapsed > 200*time.Millisecond {
		t.Errorf("elapsed %v — the loop ran to the round cap instead of the budget (%v)", elapsed, budget)
	}
	if p.calls >= 32 {
		t.Errorf("model rounds = %d, want fewer than the round cap (the budget stopped it)", p.calls)
	}
	if len(resp.Content) == 0 {
		t.Error("the response carries nothing — the synthesized tool blocks must survive the budget")
	}

	var noted bool
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventEmulatedTool && strings.Contains(ev.Reason, "budget") {
			noted = true
		}
	}
	if !noted {
		t.Error("the trace does not name the exhausted loop budget — the log must say whether the proxy stopped waiting")
	}
}
