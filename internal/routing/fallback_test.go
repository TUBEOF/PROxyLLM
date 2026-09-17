package routing_test

// ADR-010 fallback contract tests, derived from the ADR (not implementation
// behavior):
//
//   - Vision fallback: a vision-needing request whose rule chain filters to
//     nothing consults the configured fallback chain; the consultation and
//     every appended entry are traced; an empty configured chain keeps the
//     documented ErrNoRoute.
//   - EmulatorPool: a failing emulator fails over to the next in the family
//     list; the first success answers; all-failed surfaces the last error.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// capProvider is a named provider with a fixed capability set.
type capProvider struct {
	id   string
	caps provider.Capabilities
}

func (p *capProvider) ID() string                          { return p.id }
func (p *capProvider) Type() string                        { return "custom-openai" }
func (p *capProvider) Capabilities() provider.Capabilities { return p.caps }
func (p *capProvider) Complete(_ context.Context, _ ir.Request) (ir.Response, error) {
	return ir.Response{}, nil
}
func (p *capProvider) Stream(_ context.Context, _ ir.Request) (<-chan ir.StreamEvent, func(), error) {
	return nil, func() {}, nil
}
func (p *capProvider) RefreshCredentials(_ context.Context) error { return nil }

func TestVisionFallback_AppendsFallbackChain(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register(&capProvider{id: "chat-only", caps: provider.Capabilities{Streaming: true, Tools: true}})
	reg.Register(&capProvider{id: "vision-pro", caps: provider.Capabilities{Vision: true}})

	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{{MatchModel: "gpt-5", Chain: []string{"chat-only"}, Enabled: true}},
	}, routing.WithRegistry(reg))
	r.SetVisionFallback([]string{"vision-pro"})

	chain, trace, err := r.ResolveChainFor("gpt-5", provider.Capabilities{Vision: true})
	if err != nil {
		t.Fatalf("ResolveChainFor: %v", err)
	}
	if len(chain) != 1 || chain[0].ProviderID != "vision-pro" {
		t.Errorf("chain = %+v, want the vision fallback entry", chain)
	}
	// Never silent: the consultation event must be in the trace.
	sawFallback := false
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventCapabilityFallback {
			sawFallback = true
		}
	}
	if !sawFallback {
		t.Errorf("trace lacks the capability_fallback event: %+v", trace.Events())
	}
}

func TestVisionFallback_TypeEntryExpandsToAccounts(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register(&capProvider{id: "acc-1", caps: provider.Capabilities{Vision: true}})
	reg.Register(&capProvider{id: "acc-2", caps: provider.Capabilities{Vision: true}})

	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{{MatchModel: "*", Chain: []string{"nonexistent"}, Enabled: true}},
	}, routing.WithRegistry(reg))
	r.SetVisionFallback([]string{"custom-openai"})

	// The rule chain names an unregistered provider (filtered out); the
	// fallback type entry expands to both accounts.
	chain, _, err := r.ResolveChainFor("any-model", provider.Capabilities{Vision: true})
	if err != nil {
		t.Fatalf("ResolveChainFor: %v", err)
	}
	if len(chain) != 2 || chain[0].ProviderID != "acc-1" || chain[1].ProviderID != "acc-2" {
		t.Errorf("type expansion order = %+v, want [acc-1 acc-2]", chain)
	}
}

func TestVisionFallback_EmptyConfiguredChainIsErrNoRoute(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register(&capProvider{id: "chat-only", caps: provider.Capabilities{Streaming: true}})

	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{{MatchModel: "gpt-5", Chain: []string{"chat-only"}, Enabled: true}},
	}, routing.WithRegistry(reg))
	// No SetVisionFallback: the documented ErrNoRoute behavior, unchanged.

	if _, _, err := r.ResolveChainFor("gpt-5", provider.Capabilities{Vision: true}); !errors.Is(err, routing.ErrNoRoute) {
		t.Errorf("no fallback configured: err = %v, want ErrNoRoute", err)
	}
}

func TestVisionFallback_FallbackEntryLackingVisionIsSkipped(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register(&capProvider{id: "vision-pro", caps: provider.Capabilities{Vision: true}})
	reg.Register(&capProvider{id: "no-vision", caps: provider.Capabilities{}})

	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{{MatchModel: "*", Chain: []string{"chat-only"}, Enabled: true}},
	}, routing.WithRegistry(reg))
	r.SetVisionFallback([]string{"no-vision", "vision-pro"})

	chain, _, err := r.ResolveChainFor("m", provider.Capabilities{Vision: true})
	if err != nil {
		t.Fatalf("ResolveChainFor: %v", err)
	}
	if len(chain) != 1 || chain[0].ProviderID != "vision-pro" {
		t.Errorf("chain = %+v, want only the vision-capable fallback entry", chain)
	}
}

// --- EmulatorPool ---

// poolEmulator records calls and fails on demand (EmulatorPool tests).
type poolEmulator struct {
	id    string
	calls int
	err   error
}

func (f *poolEmulator) Search(_ context.Context, _ string, _ ir.ServerToolConfig) ([]ir.WebSearchResultItem, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return []ir.WebSearchResultItem{{Title: "from-" + f.id}}, nil
}
func (f *poolEmulator) Fetch(_ context.Context, _ string, _ ir.ServerToolConfig) (*ir.WebFetchContent, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &ir.WebFetchContent{Title: "from-" + f.id}, nil
}

func TestEmulatorPool_SearchFailover(t *testing.T) {
	first := &poolEmulator{id: "first", err: fmt.Errorf("first down")}
	second := &poolEmulator{id: "second"}
	pool := routing.NewEmulatorPool([]routing.Emulator{first, second}, []routing.Emulator{first, second})

	res, err := pool.Search(context.Background(), "q", ir.ServerToolConfig{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 1 || res[0].Title != "from-second" {
		t.Errorf("search result = %+v, want the second emulator's answer", res)
	}
	if first.calls != 1 {
		t.Errorf("first emulator calls = %d, want 1", first.calls)
	}
	fc, err := pool.Fetch(context.Background(), "https://x", ir.ServerToolConfig{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if fc.Title != "from-second" {
		t.Errorf("fetch title = %q, want from-second", fc.Title)
	}
	if second.calls != 2 {
		t.Errorf("second emulator calls = %d, want 2 (search + fetch)", second.calls)
	}
}

func TestEmulatorPool_FirstSuccessWins(t *testing.T) {
	first := &poolEmulator{id: "first"}
	second := &poolEmulator{id: "second"}
	pool := routing.NewEmulatorPool([]routing.Emulator{first, second}, nil)

	res, err := pool.Search(context.Background(), "q", ir.ServerToolConfig{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res[0].Title != "from-first" {
		t.Errorf("result = %+v, want the first emulator's answer", res)
	}
	if second.calls != 0 {
		t.Errorf("second emulator must not be called on first success, got %d calls", second.calls)
	}
}

func TestEmulatorPool_AllFailedSurfacesLastError(t *testing.T) {
	first := &poolEmulator{id: "first", err: errors.New("first down")}
	second := &poolEmulator{id: "second", err: errors.New("second down")}
	pool := routing.NewEmulatorPool([]routing.Emulator{first, second}, nil)

	_, err := pool.Search(context.Background(), "q", ir.ServerToolConfig{})
	if err == nil || err.Error() != "second down" {
		t.Errorf("all-failed error = %v, want the LAST emulator's error", err)
	}
}
