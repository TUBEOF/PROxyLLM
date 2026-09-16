package routing

// RED tests for the router's live rule-replacement surface (plan Analysis E:
// rule state is WebUI-editable — a rule write must reach the LIVE router
// without restart). Derived from the wiring contract: SetRules swaps the
// rule set atomically, concurrent resolutions keep working, and callers
// never observe a torn state (old rules on one lookup, new on the next).
//
// The contract these tests pin:
//   - SetRules replaces the active rule set; subsequent ResolveChainFor
//     calls use the new rules.
//   - A nil/empty slice is a valid target: it de-routes everything
//     (ErrNoRoute), it does not panic.
//   - ResolveChainFor stays correct while SetRules runs concurrently
//     (atomic swap, no torn reads) — the swap is one happens-before edge,
//     not a field-by-field update.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// reloadStub satisfies the Provider contract for the reload tests; the
// registry is the only surface routing touches during chain resolution.
type reloadStub struct{ id string }

func (s *reloadStub) ID() string                                 { return s.id }
func (s *reloadStub) Capabilities() provider.Capabilities        { return provider.Capabilities{} }
func (s *reloadStub) RefreshCredentials(_ context.Context) error { return nil }
func (s *reloadStub) Complete(_ context.Context, _ ir.Request) (ir.Response, error) {
	return ir.Response{}, errors.New("stub")
}
func (s *reloadStub) Stream(_ context.Context, _ ir.Request) (<-chan ir.StreamEvent, func(), error) {
	return nil, func() {}, errors.New("stub")
}

// reloadRouter builds a router whose registry carries a live stub for every
// named provider — ResolveChainFor skips unregistered entries, so the chain
// must be backed by registered stubs to be observable.
func reloadRouter(rules []Rule, ids ...string) *Router {
	reg := provider.NewRegistry()
	for _, id := range ids {
		reg.Register(&reloadStub{id: id})
	}
	return NewRouter(Config{Rules: rules}, WithRegistry(reg))
}

// 1. A rule write replaces the live rule set: the model routed before must
// route elsewhere after SetRules.
func TestRouter_SetRules_ReplacesLiveRuleSet(t *testing.T) {
	r := reloadRouter([]Rule{
		{MatchModel: "gpt-5", Chain: []string{"prov-a"}, Enabled: true},
	}, "prov-a", "prov-b")

	chain, _, err := r.ResolveChainFor("gpt-5", provider.Capabilities{})
	if err != nil {
		t.Fatalf("before SetRules: %v", err)
	}
	if len(chain) != 1 || chain[0].ProviderID != "prov-a" {
		t.Fatalf("before SetRules: chain = %+v, want prov-a", chain)
	}

	// The write: prov-b replaces prov-a for the same model.
	r.SetRules([]Rule{
		{MatchModel: "gpt-5", Chain: []string{"prov-b"}, Enabled: true},
	})

	chain, _, err = r.ResolveChainFor("gpt-5", provider.Capabilities{})
	if err != nil {
		t.Fatalf("after SetRules: %v", err)
	}
	if len(chain) != 1 || chain[0].ProviderID != "prov-b" {
		t.Fatalf("after SetRules: chain = %+v, want prov-b", chain)
	}
}

// 2. The old rules must be fully gone after a swap: a model that only the
// first set covered must now ErrNoRoute.
func TestRouter_SetRules_OldRulesFullyReplaced(t *testing.T) {
	r := reloadRouter([]Rule{
		{MatchModel: "gpt-5", Chain: []string{"prov-a"}, Enabled: true},
	}, "prov-a")

	r.SetRules(nil)
	if _, _, err := r.ResolveChainFor("gpt-5", provider.Capabilities{}); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("after SetRules(nil): err = %v, want ErrNoRoute", err)
	}
}

// 3. Concurrent resolutions keep working while SetRules runs: no panic, no
// torn state — every observed chain is a coherent snapshot of EITHER the
// old or the new rule set (never a mix).
func TestRouter_SetRules_ConcurrentSwapIsAtomic(t *testing.T) {
	r := reloadRouter([]Rule{
		{MatchModel: "gpt-5", Chain: []string{"server-a"}, Enabled: true},
	}, "server-a", "server-b")

	setA := []Rule{
		{MatchModel: "gpt-5", Chain: []string{"server-a"}, Enabled: true},
	}
	setB := []Rule{
		{MatchModel: "gpt-5", Chain: []string{"server-b"}, Enabled: true},
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				chain, _, err := r.ResolveChainFor("gpt-5", provider.Capabilities{})
				if err != nil {
					t.Errorf("concurrent ResolveChainFor: %v", err)
					return
				}
				// Coherence: the chain reflects one whole rule set.
				if len(chain) != 1 {
					t.Errorf("torn state: chain = %+v (len != 1)", chain)
					return
				}
				if chain[0].ProviderID != "server-a" && chain[0].ProviderID != "server-b" {
					t.Errorf("torn state: unexpected provider %q", chain[0].ProviderID)
					return
				}
			}
		}()
	}
	for i := 0; i < 200; i++ {
		set := setA
		if i%2 == 1 {
			set = setB
		}
		r.SetRules(set)
	}
	close(stop)
	wg.Wait()
}
