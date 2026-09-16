package routing_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
)

// Spec: ADR-009 — chain entries are provider types OR concrete provider
// IDs; strategies round_robin and fill_first are resolved per rule with a
// global default; every expansion and rotation is a trace event. All tests
// here derive from the ADR, not from the implementation (ADR-006).

// typedStub is a stubProvider that also reports a provider type — the
// surface the router's type expansion consumes.
type typedStub struct {
	stubProvider
	typ string
}

func (s *typedStub) Type() string { return s.typ }

func seededTypeRegistry(count int) (provider.Registry, []string) {
	reg := provider.NewRegistry()
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		// id-ascending insertion order — the stable expansion order
		// ADR-009 promises (registry order = store id ASC at runtime).
		id := string(rune('a' + i))
		ids = append(ids, id)
		reg.Register(&typedStub{stubProvider: stubProvider{id: id, caps: provider.Capabilities{}}, typ: "ollama-cloud"})
	}
	return reg, ids
}

// A type entry expands to all registered accounts of that type, in stable
// registry order — and the expansion is a trace event (never silent).
func TestResolveChain_TypeExpansion(t *testing.T) {
	reg, ids := seededTypeRegistry(3)
	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{{
			ID:         "rule-1",
			MatchModel: "claude-*",
			Chain:      []string{"ollama-cloud"}, // a TYPE entry
			Enabled:    true,
		}},
	}, routing.WithRegistry(reg))

	got, trace, err := r.ResolveChainFor("claude-opus-5", provider.Capabilities{})
	if err != nil {
		t.Fatalf("ResolveChainFor: %v", err)
	}
	if len(got) != len(ids) {
		t.Fatalf("expanded chain = %v, want %v (all accounts of the type)", got, ids)
	}
	for i, e := range got {
		if e.ProviderID != ids[i] {
			t.Errorf("chain[%d] = %s, want %s (stable registry order)", i, e.ProviderID, ids[i])
		}
	}
	// The expansion is observable: From carries the type, To the accounts.
	found := false
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventExpand && ev.From == "ollama-cloud" {
			found = true
		}
	}
	if !found {
		t.Errorf("trace = %+v, want an expand event for ollama-cloud", trace.Events())
	}
}

// An exact provider ID wins over the type of the same name: legacy rows
// whose id equals a type string keep their single-provider behavior.
func TestResolveChain_ExactIDWinsOverType(t *testing.T) {
	reg, _ := seededTypeRegistry(2)
	// A legacy provider whose ID collides with the type string.
	reg.Register(&typedStub{stubProvider: stubProvider{id: "ollama-cloud", caps: provider.Capabilities{}}, typ: "ollama-cloud"})

	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{{ID: "r", MatchModel: "m", Chain: []string{"ollama-cloud"}, Enabled: true}},
	}, routing.WithRegistry(reg))

	got, _, err := r.ResolveChainFor("m", provider.Capabilities{})
	if err != nil {
		t.Fatalf("ResolveChainFor: %v", err)
	}
	if len(got) != 1 || got[0].ProviderID != "ollama-cloud" {
		t.Fatalf("chain = %v, want exactly [ollama-cloud] (ID beats type)", got)
	}
}

// A type entry with no registered accounts is a traced skip — never a
// silent drop, never a resolution crash.
func TestResolveChain_TypeWithNoAccountsSkipped(t *testing.T) {
	reg, _ := seededTypeRegistry(1)
	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{{ID: "r", MatchModel: "m", Chain: []string{"custom-openai"}, Enabled: true}}, // no accounts
	}, routing.WithRegistry(reg))

	got, trace, err := r.ResolveChainFor("m", provider.Capabilities{})
	if err == nil {
		t.Fatalf("ResolveChainFor must reject (no accounts), got chain %v", got)
	}
	found := false
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventSkipped && strings.Contains(ev.Reason, "no accounts") &&
			ev.ProviderID == "custom-openai" {
			found = true
		}
	}
	if !found {
		t.Errorf("trace = %+v, want a skip event 'no accounts for type custom-openai'", trace.Events())
	}
}

// Accounts without a credential are excluded from expansion (ADR-009
// decision 4): a dead draft would route live traffic into a guaranteed
// auth failure. The registry surface carries the flag.
func TestResolveChain_TypeExpansionSkipsCredentiallessAccounts(t *testing.T) {
	reg := provider.NewRegistry()
	withKey := &credStub{stubProvider: stubProvider{id: "with-key", caps: provider.Capabilities{}}, typ: "ollama-cloud", hasCredential: true}
	noKey := &credStub{stubProvider: stubProvider{id: "no-key", caps: provider.Capabilities{}}, typ: "ollama-cloud", hasCredential: false}
	reg.Register(withKey)
	reg.Register(noKey)

	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{{ID: "r", MatchModel: "m", Chain: []string{"ollama-cloud"}, Enabled: true}},
	}, routing.WithRegistry(reg))

	got, trace, err := r.ResolveChainFor("m", provider.Capabilities{})
	if err != nil {
		t.Fatalf("ResolveChainFor: %v", err)
	}
	if len(got) != 1 || got[0].ProviderID != "with-key" {
		t.Fatalf("chain = %v, want [with-key] (credentialless account excluded)", got)
	}
	found := false
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventSkipped && ev.ProviderID == "no-key" &&
			strings.Contains(ev.Reason, "credential") {
			found = true
		}
	}
	if !found {
		t.Errorf("trace = %+v, want a credential-skip event for no-key", trace.Events())
	}
}

// Round robin rotates the expanded candidates per request: consecutive
// resolutions for the same rule start at consecutive offsets, and the
// distribution over N requests covers all accounts. The rotation is traced.
func TestResolveChain_RoundRobinRotation(t *testing.T) {
	reg, ids := seededTypeRegistry(3)
	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{{
			ID:         "rule-rr",
			MatchModel: "m",
			Chain:      []string{"ollama-cloud"},
			Strategy:   routing.StrategyRoundRobin,
			Enabled:    true,
		}},
	}, routing.WithRegistry(reg))

	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		got, trace, err := r.ResolveChainFor("m", provider.Capabilities{})
		if err != nil {
			t.Fatalf("ResolveChainFor #%d: %v", i, err)
		}
		if len(got) != len(ids) {
			t.Fatalf("rotation must not drop candidates: got %d, want %d", len(got), len(ids))
		}
		if got[0].ProviderID != ids[i%len(ids)] {
			t.Errorf("request #%d: first = %s, want %s (round-robin order)", i, got[0].ProviderID, ids[i%len(ids)])
		}
		seen[got[0].ProviderID]++
		found := false
		for _, ev := range trace.Events() {
			if ev.Type == routing.EventRotation {
				found = true
			}
		}
		if !found {
			t.Errorf("request #%d: trace has no rotation event", i)
		}
	}
	// Even distribution: 2 requests per account over 6 round-robin turns.
	for _, id := range ids {
		if seen[id] != 2 {
			t.Errorf("account %s served %d first-slots, want 2 (even distribution)", id, seen[id])
		}
	}
}

// Rotation is per rule: two rules with independent counters do not share
// rotation state, and a rule's counter survives nothing across router
// rebuilds (per-request-sequence determinism, ADR-009 consequences).
func TestResolveChain_RoundRobinCounterIsPerRule(t *testing.T) {
	reg, ids := seededTypeRegistry(2)
	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{
			{ID: "rule-a", MatchModel: "a", Chain: []string{"ollama-cloud"}, Strategy: routing.StrategyRoundRobin, Enabled: true},
			{ID: "rule-b", MatchModel: "b", Chain: []string{"ollama-cloud"}, Strategy: routing.StrategyRoundRobin, Enabled: true},
		},
	}, routing.WithRegistry(reg))

	// rule-a rotates: request 2 starts at account 2.
	first, _, err := r.ResolveChainFor("a", provider.Capabilities{})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := r.ResolveChainFor("a", provider.Capabilities{})
	if err != nil {
		t.Fatal(err)
	}
	if second[0].ProviderID == first[0].ProviderID {
		t.Errorf("rule-a did not rotate: both requests start at %s", first[0].ProviderID)
	}

	// rule-b has its own counter: its FIRST request starts at offset 0,
	// regardless of rule-a's rotations.
	got, _, err := r.ResolveChainFor("b", provider.Capabilities{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].ProviderID != ids[0] {
		t.Errorf("rule-b's first resolution starts at %s, want %s (independent counter)", got[0].ProviderID, ids[0])
	}
}

// Rule strategy beats the global default; an empty rule strategy inherits
// it. The global default itself is round_robin (ADR-009 decision 3).
func TestResolveChain_StrategyPrecedence(t *testing.T) {
	reg, ids := seededTypeRegistry(2)

	// Inherit (empty rule strategy) + global round_robin → rotates.
	inherit := routing.NewRouter(routing.Config{
		Rules:          []routing.Rule{{ID: "r", MatchModel: "m", Chain: []string{"ollama-cloud"}, Enabled: true}},
		GlobalStrategy: routing.StrategyRoundRobin,
	}, routing.WithRegistry(reg))
	first, _, _ := inherit.ResolveChainFor("m", provider.Capabilities{})
	second, _, _ := inherit.ResolveChainFor("m", provider.Capabilities{})
	if second[0].ProviderID == first[0].ProviderID {
		t.Errorf("inherited global round_robin must rotate (got %s twice)", first[0].ProviderID)
	}

	// Explicit fill_first on the rule beats the global round_robin.
	pinned := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{{
			ID: "r", MatchModel: "m", Chain: []string{"ollama-cloud"},
			Strategy: routing.StrategyFillFirst, Enabled: true,
		}},
		GlobalStrategy: routing.StrategyRoundRobin,
	}, routing.WithRegistry(reg))
	for i := 0; i < 3; i++ {
		got, _, err := pinned.ResolveChainFor("m", provider.Capabilities{})
		if err != nil {
			t.Fatal(err)
		}
		if got[0].ProviderID != ids[0] {
			t.Errorf("fill_first request #%d starts at %s, want %s (sticky)", i, got[0].ProviderID, ids[0])
		}
	}
}

// fill_first is today's behavior, named: no rotation, ordered chain. For
// exact-ID chains nothing changes at all.
func TestResolveChain_FillFirstKeepsOrder(t *testing.T) {
	reg := provider.NewRegistry()
	for _, pid := range []string{"p1", "p2"} {
		reg.Register(&stubProvider{id: pid, caps: provider.Capabilities{}})
	}
	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{{
			ID: "r", MatchModel: "m", Chain: []string{"p1", "p2"},
			Strategy: routing.StrategyFillFirst, Enabled: true,
		}},
	}, routing.WithRegistry(reg))
	for i := 0; i < 3; i++ {
		got, _, err := r.ResolveChainFor("m", provider.Capabilities{})
		if err != nil {
			t.Fatal(err)
		}
		if got[0].ProviderID != "p1" || got[1].ProviderID != "p2" {
			t.Errorf("fill_first chain = %v, want [p1 p2] (ordered, sticky)", got)
		}
	}
}

// Rotation happens BEFORE the capability/health filter: a cooled-down
// account rotated to the front is skipped (traced), and the next candidate
// serves — the strategy never overrides the health gate.
func TestResolveChain_RoundRobinRespectsHealthGate(t *testing.T) {
	reg, ids := seededTypeRegistry(2)
	h := routing.NewHealthTracker()
	// Account 0 trips its breaker.
	for i := 0; i < 5; i++ {
		h.ReportFailure(ids[0])
	}

	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{{
			ID: "r", MatchModel: "m", Chain: []string{"ollama-cloud"},
			Strategy: routing.StrategyRoundRobin, Enabled: true,
		}},
	}, routing.WithRegistry(reg), routing.WithHealth(h))

	// Request 1 starts at account 0 (breaker open → skipped, account 1
	// serves); request 2 starts at account 1 directly.
	for i := 0; i < 2; i++ {
		got, trace, err := r.ResolveChainFor("m", provider.Capabilities{})
		if err != nil {
			t.Fatalf("ResolveChainFor #%d: %v", i, err)
		}
		if len(got) != 1 || got[0].ProviderID != ids[1] {
			t.Errorf("request #%d: chain = %v, want [%s]", i, got, ids[1])
		}
		found := false
		for _, ev := range trace.Events() {
			if ev.Type == routing.EventSkipped && ev.ProviderID == ids[0] &&
				strings.Contains(ev.Reason, "cooldown") {
				found = true
			}
		}
		if !found {
			t.Errorf("request #%d: no cooldown skip for %s in trace", i, ids[0])
		}
	}
}

// SetStrategy swaps the live global default without a restart — the same
// live-swap contract as SetRules/SetAliases (admin API reload hook).
func TestSetStrategy_LiveSwap(t *testing.T) {
	reg, _ := seededTypeRegistry(2)
	r := routing.NewRouter(routing.Config{
		Rules:          []routing.Rule{{ID: "r", MatchModel: "m", Chain: []string{"ollama-cloud"}, Enabled: true}},
		GlobalStrategy: routing.StrategyFillFirst,
	}, routing.WithRegistry(reg))

	// fill_first: sticky.
	first, _, _ := r.ResolveChainFor("m", provider.Capabilities{})
	second, _, _ := r.ResolveChainFor("m", provider.Capabilities{})
	if second[0].ProviderID != first[0].ProviderID {
		t.Fatal("fill_first must be sticky before the swap")
	}

	r.SetStrategy(routing.StrategyRoundRobin)
	// The very first round-robin resolution starts at offset 0 (the same
	// account fill_first pinned) — but the SECOND must rotate away from
	// wherever the sticky phase left the effective order.
	a, _, err := r.ResolveChainFor("m", provider.Capabilities{})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := r.ResolveChainFor("m", provider.Capabilities{})
	if err != nil {
		t.Fatal(err)
	}
	if a[0].ProviderID == b[0].ProviderID {
		t.Errorf("after SetStrategy(round_robin) the rotation must begin (both start at %s)", a[0].ProviderID)
	}
}

// credStub adds a credential flag to the stub — the surface the router
// consumes to keep credentialless accounts out of expansion.
type credStub struct {
	stubProvider
	typ           string
	hasCredential bool
}

func (s *credStub) Type() string        { return s.typ }
func (s *credStub) HasCredential() bool { return s.hasCredential }
func (s *credStub) RefreshCredentials(_ context.Context) error {
	return errors.New("not implemented in stub")
}
