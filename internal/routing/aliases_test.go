package routing_test

// Spec-derived alias tests (plan §6.4 / C6): the alias map resolves
// Claude Code aliases (e.g. ANTHROPIC_MODEL names) to configured primary
// models, is consulted per request, and is replaceable at runtime without
// restart (WebUI settings writes → SetAliases, same contract as SetRules).
// Every resolution is recorded in the Trace — never silent.

import (
	"errors"
	"testing"

	"github.com/proxyllm/proxyllm/internal/provider"
	"github.com/proxyllm/proxyllm/internal/routing"
)

func TestRouter_AliasResolvesAndTraces(t *testing.T) {
	reg := provider.NewLiveRegistry()
	reg.Register(&stubProvider{id: "p1", caps: provider.Capabilities{Streaming: true}})
	r := routing.NewRouter(routing.Config{
		Rules:   []routing.Rule{{MatchModel: "primary-model", Chain: []string{"p1"}, Priority: 100, Enabled: true}},
		Aliases: map[string]string{"claude-opus-alias": "primary-model"},
	}, routing.WithRegistry(reg))

	entries, err := r.ResolveChain("claude-opus-alias")
	if err != nil {
		t.Fatalf("ResolveChain: %v", err)
	}
	if len(entries) != 1 || entries[0].ProviderID != "p1" || entries[0].Model != "primary-model" {
		t.Fatalf("entries = %+v, want p1/primary-model (alias resolved)", entries)
	}

	_, trace, err := r.ResolveChainFor("claude-opus-alias", provider.Capabilities{}, routing.WithKeyID("k1"))
	if err != nil {
		t.Fatalf("ResolveChainFor: %v", err)
	}
	found := false
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventAlias && ev.From == "claude-opus-alias" && ev.To == "primary-model" {
			found = true
		}
	}
	if !found {
		t.Errorf("alias transition not traced: %+v", trace.Events())
	}
}

// Runtime replacement: SetAliases takes effect on the next resolution
// (no restart), and clearing the map un-resolves — both transitions are
// observable.
func TestRouter_SetAliasesReplacesLive(t *testing.T) {
	reg := provider.NewLiveRegistry()
	rules := []routing.Rule{{MatchModel: "primary-model", Chain: []string{"p1"}, Priority: 100, Enabled: true}}
	r := routing.NewRouter(routing.Config{Rules: rules}, routing.WithRegistry(reg))

	// No alias yet: the raw name rejects.
	if _, err := r.ResolveChain("alias-x"); errors.Is(err, routing.ErrNoRoute) == false {
		t.Fatalf("pre-alias: err = %v, want no route", err)
	}

	r.SetAliases(map[string]string{"alias-x": "primary-model"})
	entries, err := r.ResolveChain("alias-x")
	if err != nil {
		t.Fatalf("post-alias ResolveChain: %v", err)
	}
	if len(entries) != 1 || entries[0].ProviderID != "p1" {
		t.Fatalf("entries = %+v, want resolved via new alias", entries)
	}

	r.SetAliases(map[string]string{})
	if _, err := r.ResolveChain("alias-x"); !errors.Is(err, routing.ErrNoRoute) {
		t.Errorf("cleared alias still resolves: err = %v", err)
	}
}
