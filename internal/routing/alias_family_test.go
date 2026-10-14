package routing_test

// Spec-derived tests: Claude Code fires auxiliary requests with the Claude
// model IDs its own version pins — the WebFetch apply call and the auto-mode
// classifier run on names like claude-sonnet-4-6, claude-haiku-5-5 or the
// dated legacy spelling claude-3-5-sonnet-20241022. The WebUI's alias editor
// promises the operator maps that family by its bare name ("a client's
// sonnet/haiku family names map onto whatever model serves them here"), so a
// request without an exact alias entry also resolves through an alias key
// that appears as a token of the requested name. Exact entries win; the
// match is scoped to claude-* names; without a matching entry the documented
// no-route rejection stands — never a silent default.

import (
	"errors"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
)

func TestRouter_FamilyAliasResolvesVersionedClaudeIDs(t *testing.T) {
	reg := provider.NewLiveRegistry()
	reg.Register(&stubProvider{id: "p1"})
	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{
			{MatchModel: "gpt-oss", Chain: []string{"p1"}, Priority: 100, Enabled: true},
		},
		Aliases: map[string]string{"sonnet": "gpt-oss", "haiku": "gpt-oss"},
	}, routing.WithRegistry(reg))

	for _, model := range []string{
		"claude-sonnet-4-6",
		"claude-haiku-5-5",
		"claude-3-5-sonnet-20241022",
	} {
		entries, trace, err := r.ResolveChainFor(model, provider.Capabilities{})
		if err != nil {
			t.Fatalf("ResolveChainFor(%q): %v", model, err)
		}
		if len(entries) != 1 || entries[0].ProviderID != "p1" || entries[0].Model != "gpt-oss" {
			t.Fatalf("ResolveChainFor(%q) = %+v, want p1/gpt-oss via the family entry", model, entries)
		}
		var found bool
		for _, ev := range trace.Events() {
			if ev.Type == routing.EventAlias && ev.From == model && ev.To == "gpt-oss" {
				found = true
			}
		}
		if !found {
			t.Errorf("ResolveChainFor(%q): family resolution not traced as an alias: %+v", model, trace.Events())
		}
	}
}

// An alias entry for the exact requested name is a deliberate operator
// statement about that ID and outranks the family entry.
func TestRouter_ExactAliasBeatsFamilyEntry(t *testing.T) {
	reg := provider.NewLiveRegistry()
	reg.Register(&stubProvider{id: "p1"})
	reg.Register(&stubProvider{id: "p2"})
	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{
			{MatchModel: "exact-target", Chain: []string{"p1"}, Priority: 100, Enabled: true},
			{MatchModel: "family-target", Chain: []string{"p2"}, Priority: 100, Enabled: true},
		},
		Aliases: map[string]string{
			"claude-sonnet-4-6": "exact-target",
			"sonnet":            "family-target",
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

// Several keys may match one name ("claude-3-5-sonnet-…" against "3" and
// "sonnet"): the first matching name token, left to right, wins — one
// deterministic answer, in the trace.
func TestRouter_FamilyAliasIsDeterministicFirstToken(t *testing.T) {
	reg := provider.NewLiveRegistry()
	reg.Register(&stubProvider{id: "p1"})
	reg.Register(&stubProvider{id: "p2"})
	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{
			{MatchModel: "first-target", Chain: []string{"p1"}, Priority: 100, Enabled: true},
			{MatchModel: "second-target", Chain: []string{"p2"}, Priority: 100, Enabled: true},
		},
		Aliases: map[string]string{"3": "first-target", "sonnet": "second-target"},
	}, routing.WithRegistry(reg))

	for i := 0; i < 3; i++ {
		entries, _, err := r.ResolveChainFor("claude-3-5-sonnet-20241022", provider.Capabilities{})
		if err != nil {
			t.Fatalf("ResolveChainFor: %v", err)
		}
		if len(entries) != 1 || entries[0].Model != "first-target" {
			t.Fatalf("entries = %+v, want the first matching token's target", entries)
		}
	}
}

// The family match exists for Claude Code's naming; a name without the
// claude- prefix is never rewritten through it.
func TestRouter_FamilyMatchOnlyOnClaudeNames(t *testing.T) {
	reg := provider.NewLiveRegistry()
	reg.Register(&stubProvider{id: "p1"})
	r := routing.NewRouter(routing.Config{
		Rules:   []routing.Rule{{MatchModel: "gpt-oss", Chain: []string{"p1"}, Priority: 100, Enabled: true}},
		Aliases: map[string]string{"sonnet": "gpt-oss"},
	}, routing.WithRegistry(reg))

	if _, _, err := r.ResolveChainFor("sonnet-4-6", provider.Capabilities{}); !errors.Is(err, routing.ErrNoRoute) {
		t.Fatalf("err = %v, want no route for a non-Claude name", err)
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
		Aliases: map[string]string{"sonnet": "ghost-model"},
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
