package routing_test

// Capability-fallback contract tests, derived from the specification (not
// implementation behavior). The vision fallback is not a router-side
// pre-stream gate — static provider caps cannot know which MODEL accepts
// images (ADR-010's amendment note). The contract:
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

// poolEmulator records calls and fails — or answers nothing — on demand
// (EmulatorPool tests). empty models an account that answers successfully with
// no results: Firecrawl does that for a query with no matches and for a page it
// could not serve (metadata.statusCode inside a 200 body).
type poolEmulator struct {
	id    string
	calls int
	err   error
	empty bool
}

func (f *poolEmulator) Search(_ context.Context, _ string, _ ir.ServerToolConfig) ([]ir.WebSearchResultItem, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.empty {
		return nil, nil
	}
	return []ir.WebSearchResultItem{{Title: "from-" + f.id}}, nil
}
func (f *poolEmulator) Fetch(_ context.Context, _ string, _ ir.ServerToolConfig) (*ir.WebFetchContent, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.empty {
		return &ir.WebFetchContent{URL: "https://x", Source: "text", MediaType: "text/plain"}, nil
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

// --- Empty answers ---
//
// Spec: "web search sometimes returns no results" — an account that answers
// with nothing must not end the walk, because the next account may have the
// results. Emptiness comes from the documented wire shapes: a search with no
// matches (data.web is an empty array with success:true) and a page Firecrawl
// could not serve (an empty payload). Precedence: a non-empty answer wins, an
// empty answer is remembered and the walk continues, and the LAST error is
// surfaced only when nothing answered at all.

func TestEmulatorPool_EmptyResultContinuesTheWalk(t *testing.T) {
	first := &poolEmulator{id: "first", empty: true}
	second := &poolEmulator{id: "second"}
	pool := routing.NewEmulatorPool([]routing.Emulator{first, second}, []routing.Emulator{first, second})

	res, err := pool.Search(context.Background(), "q", ir.ServerToolConfig{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 1 || res[0].Title != "from-second" {
		t.Errorf("search result = %+v, want the second account's answer", res)
	}
	if second.calls != 1 {
		t.Errorf("second emulator calls = %d, want 1 — an empty answer must not stop the walk", second.calls)
	}

	fc, err := pool.Fetch(context.Background(), "https://x", ir.ServerToolConfig{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if fc.Title != "from-second" {
		t.Errorf("fetch title = %q, want the second account's page", fc.Title)
	}
}

func TestEmulatorPool_AllEmptyIsAnAnswerNotAnError(t *testing.T) {
	first := &poolEmulator{id: "first", empty: true}
	second := &poolEmulator{id: "second", empty: true}
	pool := routing.NewEmulatorPool([]routing.Emulator{first, second}, nil)

	res, err := pool.Search(context.Background(), "q", ir.ServerToolConfig{})
	if err != nil {
		t.Errorf("all-empty error = %v, want nil — nothing found is an answer, and the model is told so", err)
	}
	if len(res) != 0 {
		t.Errorf("result = %+v, want empty", res)
	}
	if second.calls != 1 {
		t.Errorf("second emulator calls = %d, want 1 (every account was asked)", second.calls)
	}
}

// An empty answer outranks a later failure: the walk found an answer, even if
// it was "nothing", so the caller must not see the other account's error.
func TestEmulatorPool_EmptyBeatsALaterError(t *testing.T) {
	empty := &poolEmulator{id: "empty", empty: true}
	broken := &poolEmulator{id: "broken", err: errors.New("broken down")}
	pool := routing.NewEmulatorPool([]routing.Emulator{empty, broken}, nil)

	res, err := pool.Search(context.Background(), "q", ir.ServerToolConfig{})
	if err != nil {
		t.Errorf("error = %v, want nil — an empty answer wins over a later account's failure", err)
	}
	if len(res) != 0 {
		t.Errorf("result = %+v, want the empty answer", res)
	}
}

// And a non-empty answer still wins over a later account: the walk stops.
func TestEmulatorPool_NonEmptyAnswerStopsTheWalk(t *testing.T) {
	first := &poolEmulator{id: "first"}
	second := &poolEmulator{id: "second"}
	pool := routing.NewEmulatorPool([]routing.Emulator{first, second}, nil)

	res, err := pool.Search(context.Background(), "q", ir.ServerToolConfig{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 1 || res[0].Title != "from-first" {
		t.Errorf("result = %+v, want the first account's answer", res)
	}
	if second.calls != 0 {
		t.Errorf("second emulator calls = %d, want 0 (a non-empty answer ends the walk)", second.calls)
	}
}

// TestEmulatorPool_OrderAccessor: the pool's per-family order is
// observable (test-only, additive): NewEmulatorPool pins the lists it was
// built with; SetSearch/SetFetch replace them atomically. This accessor
// is how the server wiring test asserts the stored web_search/web_fetch
// fallback lists are CONSUMED (ADR-010 decision 3 — previously stored and
// validated but never read, pool order was registry insertion order).
func TestEmulatorPool_OrderAccessor(t *testing.T) {
	a, b := &poolEmulator{id: "a"}, &poolEmulator{id: "b"}
	c, d := &poolEmulator{id: "c"}, &poolEmulator{id: "d"}
	pool := routing.NewEmulatorPool([]routing.Emulator{a, b}, []routing.Emulator{c})
	if got := pool.SearchOrder(); len(got) != 2 || got[0] != routing.Emulator(a) || got[1] != routing.Emulator(b) {
		t.Fatalf("SearchOrder = %+v", got)
	}
	if got := pool.FetchOrder(); len(got) != 1 || got[0] != routing.Emulator(c) {
		t.Fatalf("FetchOrder = %+v", got)
	}
	pool.SetSearch([]routing.Emulator{d})
	pool.SetFetch([]routing.Emulator{d, a})
	if got := pool.SearchOrder(); len(got) != 1 || got[0] != routing.Emulator(d) {
		t.Fatalf("SearchOrder after SetSearch = %+v", got)
	}
	if got := pool.FetchOrder(); len(got) != 2 || got[0] != routing.Emulator(d) || got[1] != routing.Emulator(a) {
		t.Fatalf("FetchOrder after SetFetch = %+v", got)
	}
}
