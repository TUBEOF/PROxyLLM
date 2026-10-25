package routing_test

// Implicit model routes (ADR-011): when no enabled routing
// rule matches the post-alias model name, the router consults an injected
// model lookup — a models-table row routes to that row's provider as a
// one-entry chain, traced as implicit_route. Guards from the ADR:
//
//   - Rules win: any matching rule (exact or wildcard, any priority)
//     suppresses the implicit route entirely.
//   - Key scopes win: the step-1 rejection precedes the lookup.
//   - Aliases win: the lookup sees the post-alias name.
//   - The implicit chain runs the unchanged pipeline (expansion,
//     credential gate, strategy, capability filter, health gate), so a
//     dead/uncredentialed provider yields the honest "chain empty
//     after …" rejection (ErrNoRoute) and a cooled-down provider the
//     retryable all-providers-in-health-cooldown rejection
//     (ErrChainOverloaded), each with skip events — same behavior as an
//     explicit rule.
//
// Spec source: docs/adr/ADR-011-implicit-model-routes.md.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
)

// fakeModelLookup is the injected models-table reader: map of model ID →
// owning provider ID.
type fakeModelLookup map[string]string

func (f fakeModelLookup) lookup(_ context.Context, modelID string) (string, bool) {
	pid, ok := f[modelID]
	return pid, ok
}

func newImplicitRouter(t *testing.T, rules []routing.Rule, reg provider.Registry, models fakeModelLookup, opts ...routing.Option) *routing.Router {
	t.Helper()
	all := append([]routing.Option{routing.WithRegistry(reg), routing.WithModelLookup(models.lookup)}, opts...)
	return routing.NewRouter(routing.Config{Rules: rules}, all...)
}

func TestImplicitRoute_NoRuleRowExists(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register(&stubProvider{id: "acc-1", caps: provider.Capabilities{}})
	r := newImplicitRouter(t, nil, reg, fakeModelLookup{"ollama-cloud/glm-5.3": "acc-1"})

	got, trace, err := r.ResolveChainFor("ollama-cloud/glm-5.3", provider.Capabilities{})
	if err != nil {
		t.Fatalf("ResolveChainFor: %v", err)
	}
	if len(got) != 1 || got[0].ProviderID != "acc-1" {
		t.Fatalf("chain = %v, want [acc-1] (implicit route from the model row)", got)
	}
	found := false
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventImplicitRoute && ev.From == "ollama-cloud/glm-5.3" && ev.To == "acc-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("trace = %+v, want an implicit_route event (model, provider)", trace.Events())
	}
}

func TestImplicitRoute_NoRuleNoRowStillRejected(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register(&stubProvider{id: "acc-1", caps: provider.Capabilities{}})
	r := newImplicitRouter(t, nil, reg, fakeModelLookup{})

	got, trace, err := r.ResolveChainFor("unknown-model", provider.Capabilities{})
	if err == nil {
		t.Fatalf("chain = %v, want an error (no rule, no row)", got)
	}
	rejected := false
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventRejected && strings.Contains(ev.Reason, "no matching routing rule") {
			rejected = true
		}
	}
	if !rejected {
		t.Errorf("trace = %+v, want the unchanged no-matching-rule rejection", trace.Events())
	}
}

func TestImplicitRoute_RulesWin(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register(&stubProvider{id: "acc-1", caps: provider.Capabilities{}})
	reg.Register(&stubProvider{id: "acc-2", caps: provider.Capabilities{}})
	r := newImplicitRouter(t,
		[]routing.Rule{{MatchModel: "*", Chain: []string{"acc-2"}, Priority: 1, Enabled: true}},
		reg, fakeModelLookup{"ollama-cloud/glm-5.3": "acc-1"})

	got, trace, err := r.ResolveChainFor("ollama-cloud/glm-5.3", provider.Capabilities{})
	if err != nil {
		t.Fatalf("ResolveChainFor: %v", err)
	}
	if len(got) != 1 || got[0].ProviderID != "acc-2" {
		t.Fatalf("chain = %v, want [acc-2] (the rule wins over the implicit route)", got)
	}
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventImplicitRoute {
			t.Errorf("trace = %+v, want no implicit_route event when a rule matched", trace.Events())
		}
	}
}

func TestImplicitRoute_DisabledRuleFallsBackToImplicit(t *testing.T) {
	// A disabled rule is not a matching rule (matchingRules skips it) —
	// the implicit route must fire.
	reg := provider.NewRegistry()
	reg.Register(&stubProvider{id: "acc-1", caps: provider.Capabilities{}})
	reg.Register(&stubProvider{id: "acc-2", caps: provider.Capabilities{}})
	r := newImplicitRouter(t,
		[]routing.Rule{{MatchModel: "*", Chain: []string{"acc-2"}, Enabled: false}},
		reg, fakeModelLookup{"ollama-cloud/glm-5.3": "acc-1"})

	got, _, err := r.ResolveChainFor("ollama-cloud/glm-5.3", provider.Capabilities{})
	if err != nil {
		t.Fatalf("ResolveChainFor: %v", err)
	}
	if len(got) != 1 || got[0].ProviderID != "acc-1" {
		t.Fatalf("chain = %v, want [acc-1] (disabled rule ignored, implicit route)", got)
	}
}

func TestImplicitRoute_KeyScopeWins(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register(&stubProvider{id: "acc-1", caps: provider.Capabilities{}})
	// The scope rejection must precede the lookup.
	r := routing.NewRouter(routing.Config{
		Rules:     nil,
		KeyScopes: map[string][]string{"key-1": {"other-*"}},
	}, routing.WithRegistry(reg), routing.WithModelLookup(fakeModelLookup{"ollama-cloud/glm-5.3": "acc-1"}.lookup))

	got, trace, err := r.ResolveChainFor("ollama-cloud/glm-5.3", provider.Capabilities{}, routing.WithKeyID("key-1"))
	if err == nil {
		t.Fatalf("chain = %v, want a key-scope rejection", got)
	}
	scoped := false
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventRejected && strings.Contains(ev.Reason, "key scope") {
			scoped = true
		}
		if ev.Type == routing.EventImplicitRoute {
			t.Errorf("trace = %+v, want no implicit_route event behind the key scope", trace.Events())
		}
	}
	if !scoped {
		t.Errorf("trace = %+v, want a key-scope rejection", trace.Events())
	}
}

func TestImplicitRoute_AliasResolvesFirst(t *testing.T) {
	// The alias maps the client-facing name to the routing identity;
	// the implicit lookup must see the post-alias name.
	reg := provider.NewRegistry()
	reg.Register(&stubProvider{id: "acc-1", caps: provider.Capabilities{}})
	r := routing.NewRouter(routing.Config{
		Rules:   nil,
		Aliases: map[string]string{"claude-sonnet-5": "ollama-cloud/glm-5.3"},
	}, routing.WithRegistry(reg), routing.WithModelLookup(fakeModelLookup{"ollama-cloud/glm-5.3": "acc-1"}.lookup))

	got, trace, err := r.ResolveChainFor("claude-sonnet-5", provider.Capabilities{})
	if err != nil {
		t.Fatalf("ResolveChainFor: %v", err)
	}
	if len(got) != 1 || got[0].ProviderID != "acc-1" {
		t.Fatalf("chain = %v, want [acc-1] (alias → routing id → implicit route)", got)
	}
	if len(trace.Events()) < 1 || trace.Events()[0].Type != routing.EventAlias {
		t.Errorf("trace = %+v, want the alias event first", trace.Events())
	}
}

func TestImplicitRoute_UnregisteredProviderRejects(t *testing.T) {
	// The row names a provider the registry does not know: the implicit
	// chain filters to empty — the honest rejection with the skip event,
	// identical to an explicit rule naming a dead provider.
	reg := provider.NewRegistry()
	r := newImplicitRouter(t, nil, reg, fakeModelLookup{"ollama-cloud/glm-5.3": "gone"})

	got, trace, err := r.ResolveChainFor("ollama-cloud/glm-5.3", provider.Capabilities{})
	if err == nil {
		t.Fatalf("chain = %v, want the chain-empty rejection", got)
	}
	if len(trace.Events()) == 0 || trace.Events()[0].Type != routing.EventImplicitRoute {
		t.Errorf("trace = %+v, want implicit_route first (then the skips)", trace.Events())
	}
	rejected := false
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventRejected && strings.Contains(ev.Reason, "chain empty") {
			rejected = true
		}
	}
	if !rejected {
		t.Errorf("trace = %+v, want the chain-empty-after-filtering rejection", trace.Events())
	}
}

func TestImplicitRoute_CredentialedGate(t *testing.T) {
	// The credentialless gate (ADR-009 decision 4) applies to the implicit
	// chain exactly as to an explicit rule chain.
	reg := provider.NewRegistry()
	reg.Register(&stubProvider{id: "acc-1", caps: provider.Capabilities{}})
	reg.Register(newStubCredentialed("acc-2", false))
	r := newImplicitRouter(t, nil, reg, fakeModelLookup{"ollama-cloud/glm-5.3": "acc-2"})

	got, trace, err := r.ResolveChainFor("ollama-cloud/glm-5.3", provider.Capabilities{})
	if err == nil {
		t.Fatalf("chain = %v, want the chain-empty rejection (no credential)", got)
	}
	found := false
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventSkipped && ev.ProviderID == "acc-2" &&
			strings.Contains(ev.Reason, "no credential") {
			found = true
		}
	}
	if !found {
		t.Errorf("trace = %+v, want the no-credential skip event", trace.Events())
	}
}

func TestImplicitRoute_HealthGate(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register(&stubProvider{id: "acc-1", caps: provider.Capabilities{}})

	h := routing.NewHealthTracker()
	for range 3 {
		h.ReportFailure("acc-1")
	}
	r := newImplicitRouter(t, nil, reg, fakeModelLookup{"ollama-cloud/glm-5.3": "acc-1"},
		routing.WithHealth(h))

	got, trace, err := r.ResolveChainFor("ollama-cloud/glm-5.3", provider.Capabilities{})
	if err == nil {
		t.Fatalf("chain = %v, want the chain-empty rejection (cooldown)", got)
	}
	// Spec: docs/api/errors.md (error model) — a temporarily unavailable
	// chain (health cooldown) is a distinct terminal error from a missing
	// route, so the surfaces can answer 529/503 (retryable) instead of
	// the non-retryable 404.
	if !errors.Is(err, routing.ErrChainOverloaded) {
		t.Errorf("err = %v, want ErrChainOverloaded (all providers in health cooldown)", err)
	}
	if errors.Is(err, routing.ErrNoRoute) {
		t.Errorf("err = %v, must not be ErrNoRoute (404 implies the model does not exist)", err)
	}
	// The typed facts drive the surface message and Retry-After: a breaker
	// trip names the breaker and carries the first recovery deadline.
	var overload *routing.ChainOverloadedError
	if !errors.As(err, &overload) {
		t.Errorf("err = %T, want *routing.ChainOverloadedError", err)
	} else {
		if overload.Reason != "breaker" {
			t.Errorf("reason = %q, want breaker", overload.Reason)
		}
		if overload.Until.IsZero() {
			t.Error("Until is zero, want the breaker deadline")
		} else if d := time.Until(overload.Until); d <= 0 || d > 10*time.Second {
			t.Errorf("Until is %v away, want within the first 5s cooldown", d)
		}
	}
	// The rejection reason names the cause: all providers cooled down.
	rejected := false
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventRejected && strings.Contains(ev.Reason, "health cooldown") {
			rejected = true
		}
	}
	if !rejected {
		t.Errorf("trace = %+v, want the all-providers-in-health-cooldown rejection", trace.Events())
	}
	found := false
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventSkipped && ev.ProviderID == "acc-1" &&
			strings.Contains(ev.Reason, "cooldown") {
			found = true
		}
	}
	if !found {
		t.Errorf("trace = %+v, want the cooldown skip event", trace.Events())
	}
}

// TestImplicitRoute_CapabilitySkipStaysNoRoute: a chain emptied by the
// capability filter alone is a configuration gap (404 semantics per
// docs/api/errors.md "Model not found (404)") — never the retryable
// overload error, which is reserved for health cooldown.
func TestImplicitRoute_CapabilitySkipStaysNoRoute(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register(&stubProvider{id: "acc-1", caps: provider.Capabilities{}})
	r := newImplicitRouter(t, nil, reg, fakeModelLookup{"ollama-cloud/glm-5.3": "acc-1"})

	_, trace, err := r.ResolveChainFor("ollama-cloud/glm-5.3", provider.Capabilities{Tools: true})
	if !errors.Is(err, routing.ErrNoRoute) {
		t.Errorf("err = %v, want ErrNoRoute (capability gap is configurational)", err)
	}
	if errors.Is(err, routing.ErrChainOverloaded) {
		t.Errorf("err = %v, a capability gap must not surface as overload", err)
	}
	found := false
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventSkipped && ev.Reason == "capability gap: tools" {
			found = true
		}
	}
	if !found {
		t.Errorf("trace = %+v, want the capability-gap skip event", trace.Events())
	}
}

func TestImplicitRoute_NoLookupOptionUnchanged(t *testing.T) {
	// A router built without WithModelLookup keeps the exact pre-ADR-011
	// behavior: no rule → no route (harnesses and tests that never opted
	// in are untouched).
	reg := provider.NewRegistry()
	reg.Register(&stubProvider{id: "acc-1", caps: provider.Capabilities{}})
	r := routing.NewRouter(routing.Config{Rules: nil}, routing.WithRegistry(reg))

	if _, _, err := r.ResolveChainFor("ollama-cloud/glm-5.3", provider.Capabilities{}); err == nil {
		t.Error("no lookup option: no rule must stay an error")
	}
}

// stubCredentialedProvider extends stubProvider with the credential
// presence surface the expansion gate consults (ADR-009 decision 4).
type stubCredentialedProvider struct {
	stubProvider
	credentialed bool
}

func newStubCredentialed(id string, credentialed bool) *stubCredentialedProvider {
	return &stubCredentialedProvider{stubProvider: stubProvider{id: id}, credentialed: credentialed}
}

func (s *stubCredentialedProvider) HasCredential() bool { return s.credentialed }
