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

// Routing rule-model contract tests, RED-GREEN. The rule model is
// deterministic, testable, and observable:
//
//	1. Key scope — client key's allowed model patterns.
//	2. Model match — exact, wildcard, alias map (Claude Code aliases →
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
			name:  "interior wildcard matches the literal segments in order",
			rules: []routing.Rule{rule("claude-*-4-6", []string{"p1"}, true)},
			model: "claude-sonnet-4-6",
			want:  []string{"p1"},
		},
		{
			name:    "interior wildcard keeps its anchored tail",
			rules:   []routing.Rule{rule("claude-*-4-6", []string{"p1"}, true)},
			model:   "claude-sonnet-4-7",
			wantErr: true,
		},
		{
			name:  "suffix wildcard matches the trailing segment",
			rules: []routing.Rule{rule("*-sonnet-*", []string{"p1"}, true)},
			model: "claude-3-5-sonnet-20241022",
			want:  []string{"p1"},
		},
		{
			name:    "a plain pattern never matches as a substring",
			rules:   []routing.Rule{rule("claude", []string{"p1"}, true)},
			model:   "claude-opus-5",
			wantErr: true,
		},
		{
			name: "alias resolves to the target model's rule",
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
				"claude-sonnet-5": "gpt-5", // Claude Code style alias for the test
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

// Determinism: identical inputs must produce the identical chain on every
// call, in order. Score/weight routing was rejected as the primary mechanism
// because it cannot promise that.
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

// Capability filter: chain entries whose provider lacks a required capability
// are skipped — with the skip recorded (fallbacks are never silent). Vision is
// deliberately
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

// The vision gate narrows the no-pre-stream-gate decision above, and the
// reason the comment above gives for gating being
// impossible is exactly what changed: static provider caps cannot know which
// MODEL accepts images, but a MODEL ROW can — it LEARNS vision:false from a
// real rejection. A candidate whose row knows it cannot see images is dropped
// when the request carries one, so the request goes straight to a model that
// can, instead of paying an upstream rejection plus an image caption on every
// single turn of the conversation.
//
// It is a preference among candidates, never a veto — see the three guards
// below.
func TestResolveChain_KnownBlindModelIsSkippedForImages(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register(&stubProvider{id: "blind", caps: provider.Capabilities{Streaming: true}})
	reg.Register(&stubProvider{id: "seeing", caps: provider.Capabilities{Streaming: true}})

	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{rule("claude-*", []string{"blind", "seeing"}, true)},
	}, routing.WithRegistry(reg),
		routing.WithModelVisionLookup(func(_ context.Context, providerID, _ string) (bool, bool) {
			return providerID != "blind", true
		}))

	got, trace, err := r.ResolveChainFor("claude-opus-5", provider.Capabilities{Vision: true})
	if err != nil {
		t.Fatalf("ResolveChainFor: %v", err)
	}
	if len(got) != 1 || got[0].ProviderID != "seeing" {
		t.Fatalf("chain = %v, want [seeing] (the blind model drops out when the request carries an image)", got)
	}
	// Fallbacks are never silent.
	found := false
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventSkipped && ev.ProviderID == "blind" {
			found = true
		}
	}
	if !found {
		t.Errorf("trace = %+v, want a skip event naming the blind model", trace.Events())
	}
}

// Guard 1: an UNKNOWN vision capability is permissive. Most rows have no
// opinion, and treating "unknown" as "blind" would route around every model
// the sync has not classified yet.
func TestResolveChain_UnknownVisionKeepsEveryCandidate(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register(&stubProvider{id: "a", caps: provider.Capabilities{Streaming: true}})
	reg.Register(&stubProvider{id: "b", caps: provider.Capabilities{Streaming: true}})

	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{rule("claude-*", []string{"a", "b"}, true)},
	}, routing.WithRegistry(reg),
		routing.WithModelVisionLookup(func(context.Context, string, string) (bool, bool) {
			return false, false // no row / no opinion
		}))

	got, _, err := r.ResolveChainFor("claude-opus-5", provider.Capabilities{Vision: true})
	if err != nil {
		t.Fatalf("ResolveChainFor: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("chain = %v, want both candidates (unknown vision is not blind)", got)
	}
}

// Guard 2: the gate may never EMPTY a chain. Every candidate being known-blind
// is exactly the case the image-caption rescue exists for — the request has to
// go out, be rejected with capability_gap, and be rescued. Turning that into a
// hard no-route would be a regression dressed as an optimisation.
func TestResolveChain_AllCandidatesBlindKeepsTheChain(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register(&stubProvider{id: "blind-a", caps: provider.Capabilities{Streaming: true}})
	reg.Register(&stubProvider{id: "blind-b", caps: provider.Capabilities{Streaming: true}})

	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{rule("claude-*", []string{"blind-a", "blind-b"}, true)},
	}, routing.WithRegistry(reg),
		routing.WithModelVisionLookup(func(context.Context, string, string) (bool, bool) {
			return false, true
		}))

	got, _, err := r.ResolveChainFor("claude-opus-5", provider.Capabilities{Vision: true})
	if err != nil {
		t.Fatalf("ResolveChainFor: %v (the caption rescue must stay reachable)", err)
	}
	if len(got) != 2 {
		t.Fatalf("chain = %v, want both candidates kept (the gate never empties a chain)", got)
	}
}

// Guard 3: a request with no image is unaffected — the gate is about image
// input, not about the model's general ability.
func TestResolveChain_NoImageKeepsEveryCandidate(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register(&stubProvider{id: "blind", caps: provider.Capabilities{Streaming: true}})
	reg.Register(&stubProvider{id: "seeing", caps: provider.Capabilities{Streaming: true}})

	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{rule("claude-*", []string{"blind", "seeing"}, true)},
	}, routing.WithRegistry(reg),
		routing.WithModelVisionLookup(func(context.Context, string, string) (bool, bool) {
			return false, true
		}))

	got, _, err := r.ResolveChainFor("claude-opus-5", provider.Capabilities{})
	if err != nil {
		t.Fatalf("ResolveChainFor: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("chain = %v, want both candidates (no image, no gate)", got)
	}
}

// Health gate: providers whose breaker is open are
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

// Key scope: a client key's allowed model patterns
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

// Key-scope patterns share the routing glob semantics: an interior `*`
// matches segment-wise, and a pattern without `*` is exact only — so a
// scope can never accidentally allow a name it merely contains.
func TestResolveChain_KeyScopeGlob(t *testing.T) {
	reg := provider.NewLiveRegistry()
	reg.Register(&stubProvider{id: "p1"})
	r := routing.NewRouter(routing.Config{
		Rules:     []routing.Rule{rule("*", []string{"p1"}, true)},
		KeyScopes: map[string][]string{"key-1": {"claude-*-4-6"}},
	}, routing.WithRegistry(reg))

	if _, _, err := r.ResolveChainFor("claude-sonnet-4-6", provider.Capabilities{}, routing.WithKeyID("key-1")); err != nil {
		t.Fatalf("in-scope model must route: %v", err)
	}
	if _, _, err := r.ResolveChainFor("claude-haiku-4-5", provider.Capabilities{}, routing.WithKeyID("key-1")); !errors.Is(err, routing.ErrNoRoute) {
		t.Fatalf("err = %v, want the key-scope rejection", err)
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
