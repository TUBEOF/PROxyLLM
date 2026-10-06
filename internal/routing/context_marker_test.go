package routing_test

import (
	"testing"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
)

// Claude Code appends the context-window marker "[1m]"/"[1M]" to model
// names to declare a 1M-context override. The marker is a client-side
// hint, never part of the model identity: the deployed instance answered
// 404 not_found_error "No route for the requested model" for
// "ollama-cloud/glm-5.3-flash[1m]" (live repro 2026-10-06). Routing must
// strip the marker to the base model, record a "context_override" trace
// event (never silent), and pass the base name down the chain.
//
// Spec decision: the marker is never part of the model identity, so a
// rule pattern written WITH the marker ("...[1m]") does not match a
// request with the marker — the strip happens first, and matchers must
// express the base name.

func TestResolveChain_StripsContextMarker(t *testing.T) {
	rules := []routing.Rule{
		{MatchModel: "glm-5.3-flash", Chain: []string{"prov-a"}, Enabled: true},
		// Exact-suffix rule must NOT match: the marker is stripped before
		// rule matching, so this pattern can never win.
		{MatchModel: "glm-5.3-flash[1m]", Chain: []string{"prov-suffix"}, Enabled: true},
	}
	reg := provider.NewRegistry()
	for _, pid := range []string{"prov-a", "prov-suffix"} {
		reg.Register(&stubProvider{id: pid})
	}
	rr := routing.NewRouter(routing.Config{Rules: rules}, routing.WithRegistry(reg))

	for _, model := range []string{
		"glm-5.3-flash[1m]",
		"glm-5.3-flash[1M]", // Claude Code appends case-insensitively
	} {
		entries, trace, err := rr.ResolveChainFor(model, provider.Capabilities{})
		if err != nil {
			t.Fatalf("ResolveChainFor(%q): %v", model, err)
		}
		if len(entries) != 1 || entries[0].ProviderID != "prov-a" {
			t.Fatalf("ResolveChainFor(%q) chain = %v, want [prov-a]", model, entries)
		}
		if entries[0].Model != "glm-5.3-flash" {
			t.Errorf("ResolveChainFor(%q) entry model = %q, want base name", model, entries[0].Model)
		}
		var found bool
		for _, ev := range trace.Events() {
			if ev.Type == routing.EventContextOverride {
				if ev.From != model || ev.To != "glm-5.3-flash" {
					t.Errorf("context_override event from=%q to=%q, want from=%q to=glm-5.3-flash", ev.From, ev.To, model)
				}
				found = true
			}
		}
		if !found {
			t.Errorf("ResolveChainFor(%q): no context_override event in trace %v", model, trace.Events())
		}
	}
}

// StripContextMarker is the shared strip helper (exported so the
// client-facing surfaces can echo/log the base name consistently).
func TestStripContextMarker(t *testing.T) {
	cases := []struct {
		in, base  string
		hadMarker bool
	}{
		{"glm-5.3-flash[1m]", "glm-5.3-flash", true},
		{"glm-5.3-flash[1M]", "glm-5.3-flash", true},
		{"[1m]" + "glm-5.3-flash", "[1m]" + "glm-5.3-flash", false},
		{"glm-5.3-flash", "glm-5.3-flash", false},
		{"", "", false},
		{"[1m]", "", true},
		{"sonnet-5[1m]suffix", "sonnet-5[1m]suffix", false},
	}
	for _, tc := range cases {
		base, had := routing.StripContextMarker(tc.in)
		if base != tc.base || had != tc.hadMarker {
			t.Errorf("StripContextMarker(%q) = (%q, %v), want (%q, %v)", tc.in, base, had, tc.base, tc.hadMarker)
		}
	}
}

// A plain base-name request must produce NO context_override event.
func TestResolveChain_PlainNameNoOverrideEvent(t *testing.T) {
	rules := []routing.Rule{
		{MatchModel: "glm-5.3-flash", Chain: []string{"prov-a"}, Enabled: true},
	}
	reg := provider.NewRegistry()
	reg.Register(&stubProvider{id: "prov-a"})
	rr := routing.NewRouter(routing.Config{Rules: rules}, routing.WithRegistry(reg))

	_, trace, err := rr.ResolveChainFor("glm-5.3-flash", provider.Capabilities{})
	if err != nil {
		t.Fatalf("ResolveChainFor: %v", err)
	}
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventContextOverride {
			t.Errorf("unexpected context_override event for the plain name: %+v", ev)
		}
	}
}

// Key scope must not be defeated by the marker: a scope entry naming the
// BASE name must allow the suffixed request (the scope is checked against
// the typed name first — allowed patterns like "ollama-cloud/*" — then,
// when a marker was stripped, against the base name as well).
func TestResolveChain_KeyScopeAllowsSuffixedName(t *testing.T) {
	rules := []routing.Rule{
		{MatchModel: "glm-5.3-flash", Chain: []string{"prov-a"}, Enabled: true},
	}
	reg := provider.NewRegistry()
	reg.Register(&stubProvider{id: "prov-a"})
	rr := routing.NewRouter(routing.Config{
		Rules: rules,
		KeyScopes: map[string][]string{
			"key-1": {"ollama-cloud/*", "glm-5.3-flash"},
			"key-2": {"other-model/*"},
		},
	}, routing.WithRegistry(reg))

	entries, _, err := rr.ResolveChainFor("glm-5.3-flash[1m]", provider.Capabilities{}, routing.WithKeyID("key-1"))
	if err != nil {
		t.Fatalf("ResolveChainFor(suffixed) under scope allowing base: %v", err)
	}
	if len(entries) != 1 || entries[0].ProviderID != "prov-a" {
		t.Fatalf("chain = %v, want [prov-a]", entries)
	}

	// A scope allowing only OTHER patterns must still reject the
	// suffixed request (the marker must not bypass key scope).
	_, _, err = rr.ResolveChainFor("glm-5.3-flash[1m]", provider.Capabilities{}, routing.WithKeyID("key-2"))
	if err == nil {
		t.Fatal("suffixed request allowed under a scope that does not allow the model")
	}
}
