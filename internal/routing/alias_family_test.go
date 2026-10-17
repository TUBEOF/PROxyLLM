package routing_test

// Spec-derived tests: Claude Code fires auxiliary requests with the Claude
// model IDs its own version pins — the WebFetch apply call and the auto-mode
// classifier run on names like claude-sonnet-4-6, claude-haiku-5-5 or the
// dated legacy spelling claude-3-5-sonnet-20241022. The alias map answers
// those names through `*` wildcard keys (a `claude-*` or `claude-sonnet-*`
// entry covers each family without one entry per release — the mechanism
// documented for rule patterns); an exact entry always wins, among wildcard
// entries the pattern with the most literal characters wins, and ties go
// lexicographically — one deterministic answer. A bare key like `sonnet` is
// an exact alias ONLY (the retired family token match is gone); without a
// matching entry the documented no-route rejection stands — never a silent
// default.

import (
	"errors"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
)

func TestRouter_WildcardAliasResolvesVersionedClaudeIDs(t *testing.T) {
	reg := provider.NewLiveRegistry()
	reg.Register(&stubProvider{id: "p1"})
	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{
			{MatchModel: "gpt-oss", Chain: []string{"p1"}, Priority: 100, Enabled: true},
		},
		Aliases: map[string]string{"claude-sonnet-*": "gpt-oss", "claude-haiku-*": "gpt-oss"},
	}, routing.WithRegistry(reg))

	for _, model := range []string{
		"claude-sonnet-4-6",
		"claude-sonnet-4-5",
		"claude-haiku-5-5",
	} {
		entries, trace, err := r.ResolveChainFor(model, provider.Capabilities{})
		if err != nil {
			t.Fatalf("ResolveChainFor(%q): %v", model, err)
		}
		if len(entries) != 1 || entries[0].ProviderID != "p1" || entries[0].Model != "gpt-oss" {
			t.Fatalf("ResolveChainFor(%q) = %+v, want p1/gpt-oss via the wildcard entry", model, entries)
		}
		var found bool
		for _, ev := range trace.Events() {
			if ev.Type == routing.EventAlias && ev.From == model && ev.To == "gpt-oss" {
				found = true
			}
		}
		if !found {
			t.Errorf("ResolveChainFor(%q): wildcard resolution not traced as an alias: %+v", model, trace.Events())
		}
	}
}

// An alias entry for the exact requested name is a deliberate operator
// statement about that ID and outranks every wildcard entry.
func TestRouter_ExactAliasBeatsWildcardEntry(t *testing.T) {
	reg := provider.NewLiveRegistry()
	reg.Register(&stubProvider{id: "p1"})
	reg.Register(&stubProvider{id: "p2"})
	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{
			{MatchModel: "exact-target", Chain: []string{"p1"}, Priority: 100, Enabled: true},
			{MatchModel: "wildcard-target", Chain: []string{"p2"}, Priority: 100, Enabled: true},
		},
		Aliases: map[string]string{
			"claude-sonnet-4-6": "exact-target",
			"claude-*":          "wildcard-target",
		},
	}, routing.WithRegistry(reg))

	entries, _, err := r.ResolveChainFor("claude-sonnet-4-6", provider.Capabilities{})
	if err != nil {
		t.Fatalf("ResolveChainFor: %v", err)
	}
	if len(entries) != 1 || entries[0].ProviderID != "p1" || entries[0].Model != "exact-target" {
		t.Fatalf("entries = %+v, want the exact entry's target", entries)
	}
}

// Several wildcard keys may match one name ("claude-*" and
// "claude-sonnet-*" against "claude-sonnet-4-6"): the pattern with the most
// literal characters wins — one deterministic answer, in the trace.
func TestRouter_WildcardAliasMostLiteralWins(t *testing.T) {
	reg := provider.NewLiveRegistry()
	reg.Register(&stubProvider{id: "p1"})
	reg.Register(&stubProvider{id: "p2"})
	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{
			{MatchModel: "broad-target", Chain: []string{"p1"}, Priority: 100, Enabled: true},
			{MatchModel: "narrow-target", Chain: []string{"p2"}, Priority: 100, Enabled: true},
		},
		Aliases: map[string]string{"claude-*": "broad-target", "claude-sonnet-*": "narrow-target"},
	}, routing.WithRegistry(reg))

	for i := 0; i < 3; i++ {
		entries, trace, err := r.ResolveChainFor("claude-sonnet-4-6", provider.Capabilities{})
		if err != nil {
			t.Fatalf("ResolveChainFor: %v", err)
		}
		if len(entries) != 1 || entries[0].Model != "narrow-target" {
			t.Fatalf("entries = %+v, want the most literal pattern's target", entries)
		}
		var found bool
		for _, ev := range trace.Events() {
			if ev.Type == routing.EventAlias && ev.From == "claude-sonnet-4-6" && ev.To == "narrow-target" {
				found = true
			}
		}
		if !found {
			t.Errorf("run %d: wildcard resolution not traced: %+v", i, trace.Events())
		}
	}
}

// A bare key like `sonnet` no longer family-matches versioned IDs: it is an
// exact alias only. The operator writes `claude-sonnet-*` for the family.
func TestRouter_BareKeyIsExactOnly(t *testing.T) {
	reg := provider.NewLiveRegistry()
	reg.Register(&stubProvider{id: "p1"})
	r := routing.NewRouter(routing.Config{
		Rules:   []routing.Rule{{MatchModel: "gpt-oss", Chain: []string{"p1"}, Priority: 100, Enabled: true}},
		Aliases: map[string]string{"sonnet": "gpt-oss"},
	}, routing.WithRegistry(reg))

	if _, _, err := r.ResolveChainFor("claude-sonnet-4-6", provider.Capabilities{}); !errors.Is(err, routing.ErrNoRoute) {
		t.Fatalf("err = %v, want no route: a bare key must not match claude-sonnet-4-6", err)
	}
	entries, _, err := r.ResolveChainFor("sonnet", provider.Capabilities{})
	if err != nil {
		t.Fatalf("ResolveChainFor(sonnet): %v", err)
	}
	if len(entries) != 1 || entries[0].Model != "gpt-oss" {
		t.Fatalf("entries = %+v, want the exact key's target for a literal sonnet request", entries)
	}
}

// The terminal no-route error names the model it could not route: the wire
// message is built from it ("model: <id>"), which is the shape Claude Code
// classifies as model-not-found. errors.Is keeps matching the sentinel.
func TestResolveChain_NoRouteNamesTheModel(t *testing.T) {
	r := routing.NewRouter(routing.Config{}, routing.WithRegistry(provider.NewLiveRegistry()))

	_, _, err := r.ResolveChainFor("claude-haiku-5-5", provider.Capabilities{})
	if !errors.Is(err, routing.ErrNoRoute) {
		t.Fatalf("err = %v, want ErrNoRoute", err)
	}
	var nre *routing.NoRouteError
	if !errors.As(err, &nre) || nre.Model != "claude-haiku-5-5" {
		t.Fatalf("err = %#v, want *routing.NoRouteError naming claude-haiku-5-5", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "claude-haiku-5-5") {
		t.Fatalf("err message = %q, want the model named", msg)
	}

	// An alias entry resolving to an unroutable target still ends in the
	// no-route rejection; the error names the model the pipeline failed on
	// (the rewritten target), while the trace keeps the client-facing name.
	r2 := routing.NewRouter(routing.Config{
		Aliases: map[string]string{"claude-*": "ghost-model"},
	}, routing.WithRegistry(provider.NewLiveRegistry()))
	_, trace, err := r2.ResolveChainFor("claude-sonnet-4-6", provider.Capabilities{})
	if !errors.Is(err, routing.ErrNoRoute) {
		t.Fatalf("err = %v, want ErrNoRoute", err)
	}
	if !errors.As(err, &nre) || nre.Model != "ghost-model" {
		t.Fatalf("err = %#v, want *routing.NoRouteError naming the rewritten model", err)
	}
	var aliasTraced bool
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventAlias && ev.From == "claude-sonnet-4-6" && ev.To == "ghost-model" {
			aliasTraced = true
		}
	}
	if !aliasTraced {
		t.Errorf("alias transition not traced: %+v", trace.Events())
	}
}

// AliasTarget is the exported half of resolveWithTrace step 2: callers that
// must resolve something PER LANDED MODEL (the per-model compatibility policy)
// reach the same answer the router routes on. The semantics are step 2's —
// exact entry first, then the most literal wildcard, ties lexicographic, ONE
// hop, no marker handling (the caller strips the marker first) — so the two can
// never drift apart.
func TestRouter_AliasTargetResolvesLikeRouting(t *testing.T) {
	r := routing.NewRouter(routing.Config{Aliases: map[string]string{
		"claude-haiku-5-5": "exact-target",
		"claude-*":         "family-target",
		"claude-haiku-*":   "haiku-target",
		"a*":               "short",
		"ab*":              "longer",
		"ab*cd":            "tie-b",
		"ab*ce":            "tie-a", // same literal count as ab*cd: lexicographic winner
	}})

	for _, tc := range []struct {
		model, want string
		ok          bool
	}{
		{"claude-haiku-5-5", "exact-target", true}, // exact outranks every wildcard
		{"claude-haiku-9-9", "haiku-target", true}, // most literal characters wins
		{"claude-sonnet-4-6", "family-target", true},
		{"abXYZce", "tie-a", true}, // literal tie resolved lexicographically
		{"gpt-oss:120b", "", false},
		{"claude", "", false}, // a bare key is an exact alias only
	} {
		got, ok := r.AliasTarget(tc.model)
		if got != tc.want || ok != tc.ok {
			t.Errorf("AliasTarget(%q) = (%q, %v), want (%q, %v)", tc.model, got, ok, tc.want, tc.ok)
		}
	}

	// The live-swap contract: a settings write reaches the lookup without
	// restart (same contract as the routing it mirrors).
	r.SetAliases(map[string]string{"claude-haiku-5-5": "rewritten"})
	if got, ok := r.AliasTarget("claude-haiku-5-5"); got != "rewritten" || !ok {
		t.Fatalf("after SetAliases: (%q, %v), want (rewritten, true)", got, ok)
	}

	// Nil receiver: the fixture/test shape must not panic.
	var nilRouter *routing.Router
	if got, ok := nilRouter.AliasTarget("claude-haiku-5-5"); got != "" || ok {
		t.Fatalf("nil router: (%q, %v), want the no-match answer", got, ok)
	}
}

// The alias hop is shared with routing, so a request that routes by alias and
// the policy resolved for it name the same model — pinned here because a
// second, drifting implementation of alias resolution is silent by definition.
func TestRouter_AliasTargetAgreesWithTheResolvedChain(t *testing.T) {
	reg := provider.NewLiveRegistry()
	reg.Register(&stubProvider{id: "p1"})
	r := routing.NewRouter(routing.Config{
		Rules:   []routing.Rule{{MatchModel: "ollama-cloud/m", Chain: []string{"p1"}, Priority: 100, Enabled: true}},
		Aliases: map[string]string{"claude-*": "ollama-cloud/m"},
	}, routing.WithRegistry(reg))

	target, ok := r.AliasTarget("claude-haiku-5-5")
	if !ok || target != "ollama-cloud/m" {
		t.Fatalf("AliasTarget = (%q, %v), want (ollama-cloud/m, true)", target, ok)
	}
	entries, _, err := r.ResolveChainFor("claude-haiku-5-5", provider.Capabilities{})
	if err != nil {
		t.Fatalf("ResolveChainFor: %v", err)
	}
	if len(entries) != 1 || entries[0].Model != target {
		t.Fatalf("the routed model %+v does not match the alias target %q", entries, target)
	}
}
