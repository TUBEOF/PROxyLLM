package routing_test

import (
	"context"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// Full-pipeline routing determinism suite, RED-GREEN. Table-driven by
// scenario: each one must resolve to a FIXED event sequence for identical
// inputs — capability fallback, health cooldown, mid-stream no-swap, and
// compat-policy application incl. thinking strip/map/block.
//
// Expectations derive from the documented pipeline contract, never from
// executor/router internals:
//
//   - The pipeline order is key scope → alias → rule → capability → health;
//     compat policy applies to the request before dispatch; the executor
//     walks the chain within its retry budgets.
//   - Every skip/retry/failover/degradation is a trace event — never
//     silent. Identical inputs MUST yield the identical event-type
//     sequence on every run (score/weight fuzz was rejected for exactly
//     this reason).
//
// The stubs (stubProvider, scriptedProvider, regOf, upErr) live in the
// sibling rules_test.go / executor_test.go files of this package.

// pipelineCase is one row of the determinism table: a full routing
// situation and the exact observable outcome.
type pipelineCase struct {
	name string

	// setup
	rules     []routing.Rule
	aliases   map[string]string
	providers []provider.Provider
	// healthFailures pre-loads breaker failures for a provider ID.
	healthFailures map[string]int
	compat         routing.Compat
	request        ir.Request
	required       provider.Capabilities
	streaming      bool

	// wantEventTypes is the EXACT event-type sequence across both trace
	// surfaces (compat trace first, then the resolve+execute trace) —
	// the determinism pin. nil = not asserted (err-only row).
	wantEventTypes []routing.EventType
	// wantErr asserts a terminal pipeline error (compat block, no route,
	// exhausted chain).
	wantErr bool
	// wantAttempted asserts which provider IDs saw an upstream attempt
	// (empty slice asserts none).
	wantAttempted []string
}

// runPipeline drives the full pipeline for one table row: resolve chain
// (rules + capability + health) → apply compat → execute (complete or
// stream). It returns the combined event-type sequence and the providers
// that received an attempt.
func runPipeline(t *testing.T, tc pipelineCase) ([]routing.EventType, []string, error) {
	t.Helper()

	reg := regOf(tc.providers...)
	h := routing.NewHealthTracker()
	now := fixedNow()
	h.SetClock(func() time.Time { return now })
	for pid, n := range tc.healthFailures {
		for i := 0; i < n; i++ {
			h.ReportFailure(pid)
		}
	}

	router := routing.NewRouter(routing.Config{
		Rules:   tc.rules,
		Aliases: tc.aliases,
	}, routing.WithRegistry(reg), routing.WithHealth(h))

	req := tc.request
	required := tc.required

	chainEntries, resolveTrace, err := router.ResolveChainFor(req.Model, required)
	if err != nil {
		// Resolution failures are part of the observable outcome; the
		// caller asserts the rejected event + error.
		return eventTypes(resolveTrace), nil, err
	}

	// Compat policy applies to the request BEFORE dispatch; every
	// application rides its own trace.
	adapted, compatTrace, err := routing.ApplyCompat(tc.compat, withChainModel(chainEntries, req))
	if err != nil {
		return append(eventTypes(compatTrace), eventTypes(resolveTrace)...), nil, err
	}

	exec := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     reg,
		Health:       h,
		RetrySleeper: func(time.Duration) {},
	})

	var events []routing.EventType
	if tc.streaming {
		ch, cancel, execTrace, err := exec.ExecuteStream(context.Background(), chainEntries, adapted)
		if err != nil {
			return concatEvents(compatTrace, resolveTrace, execTrace), nil, err
		}
		defer cancel()
		for range ch {
		}
		events = concatEvents(compatTrace, resolveTrace, execTrace)
	} else {
		_, execTrace, err := exec.Execute(context.Background(), chainEntries, adapted)
		if err != nil {
			return concatEvents(compatTrace, resolveTrace, execTrace), nil, err
		}
		events = concatEvents(compatTrace, resolveTrace, execTrace)
	}
	return events, attempted(tc.providers), nil
}

// concatEvents joins the event-type sequences of the compat, resolve, and
// execute traces in pipeline order.
func concatEvents(traces ...*routing.Trace) []routing.EventType {
	var out []routing.EventType
	for _, tr := range traces {
		out = append(out, eventTypes(tr)...)
	}
	return out
}

// eventTypes extracts the event-type sequence of a trace.
func eventTypes(tr *routing.Trace) []routing.EventType {
	evs := tr.Events()
	out := make([]routing.EventType, len(evs))
	for i, ev := range evs {
		out[i] = ev.Type
	}
	return out
}

// attempted returns the provider IDs that saw at least one upstream call.
func attempted(provs []provider.Provider) []string {
	var out []string
	for _, p := range provs {
		if s, ok := p.(interface{ Calls() int }); ok && s.Calls() > 0 {
			out = append(out, p.ID())
		}
	}
	return out
}

// withChainModel stamps the resolved chain's model onto the request the
// way the httpapi layer does (the chain entry names the per-provider
// model; the executor re-stamps per attempt).
func withChainModel(chain []routing.ChainEntry, req ir.Request) ir.Request {
	if len(chain) > 0 && chain[0].Model != "" {
		req.Model = chain[0].Model
	}
	return req
}

// fixedNow is a frozen clock for the health tracker (deterministic
// cooldown decisions, no sleeps).
var fixedNow = func() time.Time { return time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC) }

// TestRoutingDeterminism_Pipeline is the gate table: each row is one
// named scenario; the event sequence is the pin.
func TestRoutingDeterminism_Pipeline(t *testing.T) {
	visionCaps := provider.Capabilities{Streaming: true, Vision: true}
	textCaps := provider.Capabilities{Streaming: true}

	cases := []pipelineCase{
		{
			// Vision is no longer gated at resolve time — the request
			// dispatches to the chain's first entry and the upstream decides.
			// A real capability_gap rejection would fail over to the executor's
			// fallback models (pinned in capabilityfallback_test.go); here
			// the text-only provider succeeds, so no fallback fires.
			name: "vision request: no pre-stream vision gate, the first chain entry serves",
			rules: []routing.Rule{
				{MatchModel: "claude-*", Chain: []string{"text-p", "vision-p"}, Enabled: true},
			},
			providers: []provider.Provider{
				&scriptedProvider{id: "text-p", caps: textCaps},
				&scriptedProvider{id: "vision-p", caps: visionCaps},
			},
			request:  ir.Request{Model: "claude-opus-5"},
			required: provider.Capabilities{Vision: true},
			// resolve: no skip (no vision gate) → executor: attempt text-p → success.
			wantEventTypes: []routing.EventType{
				routing.EventAttempt,
			},
			wantAttempted: []string{"text-p"},
		},
		{
			name: "health cooldown: cooled-down provider is skipped, the fallback serves",
			rules: []routing.Rule{
				{MatchModel: "claude-*", Chain: []string{"hot", "cold"}, Enabled: true},
			},
			providers: []provider.Provider{
				&scriptedProvider{id: "hot"},
				&scriptedProvider{id: "cold"},
			},
			healthFailures: map[string]int{"cold": 3}, // breaker opens at 3
			request:        ir.Request{Model: "claude-opus-5"},
			// resolve: skip cold (cooldown) → executor: attempt hot → success.
			wantEventTypes: []routing.EventType{
				routing.EventSkipped,
				routing.EventAttempt,
			},
			wantAttempted: []string{"hot"},
		},
		{
			name: "mid-stream no-swap: failure after the first event is terminal, the chain is never swapped",
			rules: []routing.Rule{
				{MatchModel: "claude-*", Chain: []string{"flaky", "backup"}, Enabled: true},
			},
			providers: []provider.Provider{
				&scriptedProvider{id: "flaky", streamScript: [][]ir.StreamEvent{{
					{Type: ir.Start, Response: &ir.Response{ID: "s"}},
					{Type: ir.Error, Error: &ir.UpstreamError{Message: "mid-stream"}},
				}}},
				&scriptedProvider{id: "backup"},
			},
			request:   ir.Request{Model: "claude-opus-5"},
			streaming: true,
			// dispatch succeeds on flaky (one attempt); the failure rides
			// the channel, not the trace; backup is NEVER attempted.
			wantEventTypes: []routing.EventType{
				routing.EventAttempt,
			},
			wantAttempted: []string{"flaky"},
		},
		{
			name: "compat block: a thinking request against a block policy terminates before any upstream attempt",
			rules: []routing.Rule{
				{MatchModel: "gpt-*", Chain: []string{"gp"}, Enabled: true},
			},
			providers: []provider.Provider{
				&scriptedProvider{id: "gp"},
			},
			compat:  routing.Compat{ThinkingHandling: routing.ThinkingBlock},
			request: ir.Request{Model: "gpt-5", Thinking: &ir.ThinkingConfig{Enabled: true, BudgetTokens: 2048}},
			// compat trace carries the block event; resolution is clean;
			// no attempt ever runs.
			wantEventTypes: []routing.EventType{
				routing.EventCompatPolicy,
			},
			wantErr:       true,
			wantAttempted: []string{},
		},
		{
			name: "compat strip: thinking is removed and traced, the request still routes",
			rules: []routing.Rule{
				{MatchModel: "gpt-*", Chain: []string{"gp"}, Enabled: true},
			},
			providers: []provider.Provider{
				&scriptedProvider{id: "gp"},
			},
			compat:  routing.Compat{ThinkingHandling: routing.ThinkingStrip},
			request: ir.Request{Model: "gpt-5", Thinking: &ir.ThinkingConfig{Enabled: true, BudgetTokens: 2048}},
			// compat: strip event → resolve: clean → executor: one attempt.
			wantEventTypes: []routing.EventType{
				routing.EventCompatPolicy,
				routing.EventAttempt,
			},
			wantAttempted: []string{"gp"},
		},
		{
			name: "compat map: the thinking budget maps to reasoning effort and is traced",
			rules: []routing.Rule{
				{MatchModel: "gpt-*", Chain: []string{"gp"}, Enabled: true},
			},
			providers: []provider.Provider{
				&scriptedProvider{id: "gp"},
			},
			compat:  routing.Compat{ThinkingHandling: routing.ThinkingMapToEffort},
			request: ir.Request{Model: "gpt-5", Thinking: &ir.ThinkingConfig{Enabled: true, BudgetTokens: 20000}},
			wantEventTypes: []routing.EventType{
				routing.EventCompatPolicy,
				routing.EventAttempt,
			},
			wantAttempted: []string{"gp"},
		},
		{
			name: "no route: an unmatched model is rejected, never silently defaulted",
			rules: []routing.Rule{
				{MatchModel: "gpt-*", Chain: []string{"gp"}, Enabled: true},
			},
			providers: []provider.Provider{
				&scriptedProvider{id: "gp"},
			},
			request: ir.Request{Model: "llama-4"},
			wantEventTypes: []routing.EventType{
				routing.EventRejected,
			},
			wantErr:       true,
			wantAttempted: []string{},
		},
		{
			name: "alias + capability + failover compose deterministically",
			rules: []routing.Rule{
				{MatchModel: "gpt-5", Chain: []string{"dead", "alive"}, Enabled: true},
			},
			aliases: map[string]string{"claude-sonnet-5": "gpt-5"}, // Claude Code style alias
			providers: []provider.Provider{
				&scriptedProvider{id: "dead", completeErrs: []error{upErr(500, "")}},
				&scriptedProvider{id: "alive"},
			},
			request: ir.Request{Model: "claude-sonnet-5"},
			// resolve: alias event → executor: attempt dead → failed →
			// failover → attempt alive.
			wantEventTypes: []routing.EventType{
				routing.EventAlias,
				routing.EventAttempt,
				routing.EventFailed,
				routing.EventFailover,
				routing.EventAttempt,
			},
			wantAttempted: []string{"dead", "alive"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotEvents, gotAttempted, err := runPipeline(t, tc)

			if tc.wantErr && err == nil {
				t.Fatalf("pipeline must terminate with an error, got none (events %v)", gotEvents)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("pipeline error: %v", err)
			}

			if tc.wantEventTypes != nil {
				if len(gotEvents) != len(tc.wantEventTypes) {
					t.Fatalf("event sequence = %v, want exactly %v", gotEvents, tc.wantEventTypes)
				}
				for i, want := range tc.wantEventTypes {
					if gotEvents[i] != want {
						t.Errorf("event[%d] = %s, want %s (all: %v)", i, gotEvents[i], want, gotEvents)
					}
				}
			}

			if tc.wantAttempted != nil {
				if len(gotAttempted) != len(tc.wantAttempted) {
					t.Fatalf("attempted providers = %v, want %v", gotAttempted, tc.wantAttempted)
				}
				for i := range gotAttempted {
					if gotAttempted[i] != tc.wantAttempted[i] {
						t.Errorf("attempted[%d] = %s, want %s", i, gotAttempted[i], tc.wantAttempted[i])
					}
				}
			}
		})
	}
}

// TestRoutingDeterminism_IdenticalRuns proves the determinism claim the
// rejected alternatives failed: the same rules, registry, health state,
// and request produce the SAME event-type sequence on every run — ten
// consecutive pipeline runs included.
func TestRoutingDeterminism_IdenticalRuns(t *testing.T) {
	buildCase := func() pipelineCase {
		return pipelineCase{
			rules: []routing.Rule{
				{MatchModel: "gpt-*", Chain: []string{"no-vision", "vision", "cooling"}, Enabled: true},
			},
			providers: []provider.Provider{
				&stubProvider{id: "no-vision", caps: provider.Capabilities{Streaming: true}},
				&scriptedProvider{id: "vision", caps: provider.Capabilities{Streaming: true, Vision: true}},
				&scriptedProvider{id: "cooling"},
			},
			healthFailures: map[string]int{"cooling": 3},
			compat:         routing.Compat{ThinkingHandling: routing.ThinkingStrip},
			request:        ir.Request{Model: "gpt-5", Thinking: &ir.ThinkingConfig{Enabled: true, BudgetTokens: 2048}},
			required:       provider.Capabilities{Vision: true},
		}
	}

	first, _, err := runPipeline(t, buildCase())
	if err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	for i := 0; i < 10; i++ {
		again, _, err := runPipeline(t, buildCase())
		if err != nil {
			t.Fatalf("pipeline run #%d: %v", i+1, err)
		}
		if len(again) != len(first) {
			t.Fatalf("run #%d: event count %d, want %d (sequence %v vs %v)", i+1, len(again), len(first), again, first)
		}
		for j := range again {
			if again[j] != first[j] {
				t.Fatalf("run #%d: event[%d] = %s, want %s (sequence %v vs %v)", i+1, j, again[j], first[j], again, first)
			}
		}
	}
}
