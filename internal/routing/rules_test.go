package routing_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// Routing rule-model contract tests, RED-GREEN per Analysis F. The rule
// model derives from plan Analysis E (deterministic order, testable,
// observable):
//
//	1. Key scope — client key's allowed model patterns.
//	2. Model match — exact, wildcard, alias map (Claude Code aliases C6 →
//	   configured primary models).
//	3. Capability filter — from request content.
//	4. Chain resolution — ordered provider/model fallback chain.
//	5. Health gate — circuit breaker / cooldown.
//
// Determinism requirement: same rules + same request → same chain, always
// (score/weight fuzzy routing was rejected precisely because it is not).

// modelRule builds a routing rule for the test tables.
func rule(matchModel string, chain []string, enabled bool) routing.Rule {
	return routing.Rule{MatchModel: matchModel, Chain: chain, Enabled: enabled}
}

func TestResolveChain(t *testing.T) {
	cases := []struct {
		name    string
		rules   []routing.Rule
		model   string
		want    []string
		wantErr bool
	}{
		{
			name: "exact match wins over wildcard",
			rules: []routing.Rule{
				rule("claude-*", []string{"p1"}, true),
				rule("claude-opus-5", []string{"p2"}, true),
			},
			model: "claude-opus-5",
			want:  []string{"p2"},
		},
		{
			name: "priority orders rules: highest priority wins",
			rules: []routing.Rule{
				{MatchModel: "claude-*", Chain: []string{"p-low"}, Priority: 1, Enabled: true},
				{MatchModel: "claude-*", Chain: []string{"p-high"}, Priority: 10, Enabled: true},
			},
			model: "claude-opus-5",
			want:  []string{"p-high"},
		},
		{
			name:  "wildcard matches unmatched suffix",
			rules: []routing.Rule{rule("claude-*", []string{"p1"}, true)},
			model: "claude-haiku-4-5",
			want:  []string{"p1"},
		},
		{
			name: "alias resolves to the target model's rule (C6)",
			rules: []routing.Rule{
				{MatchModel: "gpt-5", Chain: []string{"codex"}, Enabled: true},
			},
			model: "claude-sonnet-5", // alias for gpt-5 in the test map
			want:  []string{"codex"},
		},
		{
			name: "disabled rule is skipped",
			rules: []routing.Rule{
				rule("claude-*", []string{"p-disabled"}, false),
				rule("claude-*", []string{"p-enabled"}, true),
			},
			model: "claude-opus-5",
			want:  []string{"p-enabled"},
		},
		{
			name:    "no matching rule is an error, never a silent default",
			rules:   []routing.Rule{rule("gpt-*", []string{"p1"}, true)},
			model:   "claude-opus-5",
			wantErr: true,
		},
		{
			name:    "empty rule list is an error",
			rules:   nil,
			model:   "anything",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// ResolveChainFor skips unregistered providers and rejects the
			// emptied chain — register every provider any rule names.
			reg := provider.NewRegistry()
			for _, r := range tc.rules {
				for _, pid := range r.Chain {
					reg.Register(&stubProvider{id: pid, caps: provider.Capabilities{}})
				}
			}
			rr := routing.NewRouter(routing.Config{Rules: tc.rules, Aliases: map[string]string{
				"claude-sonnet-5": "gpt-5", // C6-style alias for the test
			}}, routing.WithRegistry(reg))
			got, _, err := rr.ResolveChainFor(tc.model, provider.Capabilities{})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ResolveChainFor(%q) = %v, want error", tc.model, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveChainFor(%q): %v", tc.model, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ResolveChainFor(%q) = %v, want %v", tc.model, got, tc.want)
			}
			for i := range got {
				if got[i].ProviderID != tc.want[i] {
					t.Errorf("chain[%d] = %s, want %s", i, got[i].ProviderID, tc.want[i])
				}
			}
		})
	}
}

// Determinism (Analysis E rejection of score/weight routing): identical
// inputs must produce the identical chain on every call, in order.
func TestResolveChain_Deterministic(t *testing.T) {
	rules := []routing.Rule{
		{MatchModel: "claude-*", Chain: []string{"a", "b", "c"}, Priority: 1, Enabled: true},
		{MatchModel: "claude-*", Chain: []string{"x"}, Priority: 5, Enabled: true},
	}
	reg := provider.NewRegistry()
	for _, pid := range []string{"a", "b", "c", "x"} {
		reg.Register(&stubProvider{id: pid, caps: provider.Capabilities{}})
	}
	r := routing.NewRouter(routing.Config{Rules: rules}, routing.WithRegistry(reg))
	first, _, err := r.ResolveChainFor("claude-opus-5", provider.Capabilities{})
	if err != nil {
		t.Fatalf("ResolveChainFor: %v", err)
	}
	for i := 0; i < 10; i++ {
		again, _, err := r.ResolveChainFor("claude-opus-5", provider.Capabilities{})
		if err != nil {
			t.Fatalf("ResolveChainFor #%d: %v", i, err)
		}
		if len(again) != len(first) {
			t.Fatalf("call #%d: chain length %d, want %d", i, len(again), len(first))
		}
		for j := range again {
			if again[j] != first[j] {
				t.Errorf("call #%d chain[%d] = %v, want %v", i, j, again[j], first[j])
			}
		}
	}
}

// Capability filter (Analysis E step 3; spec change 2026-09-21): chain
// entries whose provider lacks a required capability are skipped — with
// the skip recorded (fallbacks are never silent). Vision is deliberately
// NOT gated here anymore: static provider caps cannot know which MODEL
// accepts images — the request goes out and a real upstream rejection
// (capability_gap) fails over to the executor's fallback models.
func TestResolveChain_CapabilityFilter(t *testing.T) {
	// Registry: provider "vision-p" has vision, "text-p" does not.
	reg := provider.NewRegistry()
	reg.Register(&stubProvider{id: "vision-p", caps: provider.Capabilities{Streaming: true, Vision: true}})
	reg.Register(&stubProvider{id: "text-p", caps: provider.Capabilities{Streaming: true}})

	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{rule("claude-*", []string{"text-p", "vision-p"}, true)},
	}, routing.WithRegistry(reg))

	got, trace, err := r.ResolveChainFor("claude-opus-5", provider.Capabilities{Vision: true})
	if err != nil {
		t.Fatalf("ResolveChainFor: %v", err)
	}
	// No pre-stream vision gate: both entries survive resolution; the
	// upstream decides, and the executor's capability fallback (pinned in
	// capabilityfallback_test.go) handles real rejections.
	if len(got) != 2 || got[0].ProviderID != "text-p" || got[1].ProviderID != "vision-p" {
		t.Fatalf("chain = %v, want [text-p vision-p] (vision is not gated pre-stream)", got)
	}
	if len(trace.Events()) != 0 {
		t.Errorf("trace = %+v, want no skip events (no vision gate)", trace.Events())
	}
}

// Health gate (Analysis E step 5): providers whose breaker is open are
// skipped, again with the skip recorded. The breaker opens after the
// configured consecutive-failure threshold (circuit breaker), and recovery
// is passive — once the cooldown lapses, the next resolution probes.
func TestResolveChain_HealthGate(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register(&stubProvider{id: "hot", caps: provider.Capabilities{}})
	reg.Register(&stubProvider{id: "cold", caps: provider.Capabilities{}})

	// Deterministic clock: no sleeps, exact cooldown behavior.
	now := time.Now()
	h := routing.NewHealthTracker()
	h.SetClock(func() time.Time { return now })

	// Below threshold: still routed.
	h.ReportFailure("cold")
	h.ReportFailure("cold")
	if h.Cooldown("cold") {
		t.Fatal("breaker must not open below the failure threshold")
	}

	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{rule("claude-*", []string{"cold", "hot"}, true)},
	}, routing.WithRegistry(reg), routing.WithHealth(h))

	got, _, err := r.ResolveChainFor("claude-opus-5", provider.Capabilities{})
	if err != nil {
		t.Fatalf("ResolveChainFor: %v", err)
	}
	if len(got) != 2 || got[0].ProviderID != "cold" {
		t.Fatalf("chain below threshold = %v, want [cold hot]", got)
	}

	// Third failure: breaker opens, cold is skipped with a trace event.
	h.ReportFailure("cold")
	got, trace, err := r.ResolveChainFor("claude-opus-5", provider.Capabilities{})
	if err != nil {
		t.Fatalf("ResolveChainFor: %v", err)
	}
	if len(got) != 1 || got[0].ProviderID != "hot" {
		t.Fatalf("chain with open breaker = %v, want [hot] (cold skipped)", got)
	}
	found := false
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventSkipped && ev.ProviderID == "cold" &&
			strings.Contains(ev.Reason, "cooldown") {
			found = true
		}
	}
	if !found {
		t.Errorf("trace = %+v, want a cooldown skip event for cold", trace.Events())
	}

	// Cooldown lapses: cold is probed again (passive recovery).
	now = now.Add(6 * time.Second)
	got, _, err = r.ResolveChainFor("claude-opus-5", provider.Capabilities{})
	if err != nil {
		t.Fatalf("ResolveChainFor: %v", err)
	}
	if len(got) != 2 || got[0].ProviderID != "cold" {
		t.Fatalf("chain after cooldown = %v, want [cold hot] (probe recovery)", got)
	}

	// Success closes the breaker entirely (backoff reset).
	h.ReportSuccess("cold")
	if h.Cooldown("cold") {
		t.Error("breaker must close on success")
	}
}

// Key scope (Analysis E step 1): a client key's allowed model patterns
// reject out-of-scope models before any rule runs.
func TestResolveChain_KeyScope(t *testing.T) {
	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{rule("*", []string{"p1"}, true)},
		KeyScopes: map[string][]string{
			"key-1": {"claude-*"},
		},
	})
	if _, _, err := r.ResolveChainFor("claude-opus-5", provider.Capabilities{}, routing.WithKeyID("key-1")); err == nil {
		t.Error("in-scope model must route")
	}
	if _, _, err := r.ResolveChainFor("gpt-5", provider.Capabilities{}, routing.WithKeyID("key-1")); err == nil {
		t.Error("out-of-scope model must be rejected (key scope)")
	}
}

// stubProvider satisfies the Provider contract for routing tests. The
// registry is the only surface routing touches during chain resolution.
type stubProvider struct {
	id   string
	caps provider.Capabilities
}

func (s *stubProvider) ID() string                                 { return s.id }
func (s *stubProvider) Capabilities() provider.Capabilities        { return s.caps }
func (s *stubProvider) RefreshCredentials(_ context.Context) error { return nil }
func (s *stubProvider) Complete(_ context.Context, _ ir.Request) (ir.Response, error) {
	return ir.Response{}, errors.New("stub")
}
func (s *stubProvider) Stream(_ context.Context, _ ir.Request) (<-chan ir.StreamEvent, func(), error) {
	return nil, func() {}, errors.New("stub")
}
