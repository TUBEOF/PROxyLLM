package routing_test

// Capability-fallback contract tests, derived from the specification (not
// implementation behavior). Spec change 2026-09-21/22 (automatic
// capabilities, documented in ADR-010's amendment note): the vision
// fallback is NO LONGER a router-side pre-stream gate — static provider
// caps cannot know which MODEL accepts images. The new contract:
//
//   - Router: a vision-needing request resolves the rule chain without any
//     vision gate (the upstream decides); an empty chain is ErrNoRoute
//     uniformly, with or without vision. The executor-side fallback (the
//     real-upstream capability_gap rescue) is pinned in
//     capabilityfallback_test.go.
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

// TestVisionResolution_NoPreStreamGate: a vision-needing request resolves
// the chain regardless of static vision caps — the model gap surfaces at
// the upstream (capability_gap class) and the executor's fallback models
// take over (their contract: capabilityfallback_test.go).
func TestVisionResolution_NoPreStreamGate(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register(&capProvider{id: "chat-only", caps: provider.Capabilities{Streaming: true, Tools: true}})

	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{{MatchModel: "gpt-5", Chain: []string{"chat-only"}, Enabled: true}},
	}, routing.WithRegistry(reg))

	chain, trace, err := r.ResolveChainFor("gpt-5", provider.Capabilities{Vision: true})
	if err != nil {
		t.Fatalf("ResolveChainFor: %v", err)
	}
	if len(chain) != 1 || chain[0].ProviderID != "chat-only" {
		t.Errorf("chain = %+v, want the chat-only entry (vision not gated)", chain)
	}
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventCapabilityFallback {
			t.Errorf("router must not consult capability fallbacks: %+v", ev)
		}
	}
}

// TestVisionResolution_EmptyChainIsErrNoRoute: with the router-side gate
// gone, a dead chain (nothing resolves) is ErrNoRoute uniformly — with and
// without a vision requirement (the executor fallback only fires after real
// upstream rejections, never to rescue an unresolvable chain).
func TestVisionResolution_EmptyChainIsErrNoRoute(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register(&capProvider{id: "chat-only", caps: provider.Capabilities{Streaming: true}})

	// The rule chain names an unregistered provider: expansion is dead.
	r := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{{MatchModel: "gpt-5", Chain: []string{"nonexistent"}, Enabled: true}},
	}, routing.WithRegistry(reg))

	if _, _, err := r.ResolveChainFor("gpt-5", provider.Capabilities{Vision: true}); !errors.Is(err, routing.ErrNoRoute) {
		t.Errorf("vision request against dead chain: err = %v, want ErrNoRoute", err)
	}
	if _, _, err := r.ResolveChainFor("gpt-5", provider.Capabilities{}); !errors.Is(err, routing.ErrNoRoute) {
		t.Errorf("plain request against dead chain: err = %v, want ErrNoRoute", err)
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
