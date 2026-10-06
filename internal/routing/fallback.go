package routing

import (
	"context"
	"sync"

	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// ADR-010 capability fallbacks: two complementary mechanisms.
//
//   - EmulatorPool: the executor's web-tool emulator (§6.2) becomes a
//     priority list per tool family. A failing emulator fails over to the
//     next in the list — multiple firecrawl accounts (or future providers)
//     give web_search/web_fetch the same retry discipline chat chains have.
//   - CapabilityFallbacks on the Executor (2026-09-21 automatic
//     capabilities): when the whole chain failed with capability_gap
//     rejections (the model cannot serve an input modality, e.g. images),
//     the configured fallback MODELS are tried as additional chain entries
//     — see Executor.SetCapabilityFallbacks and walkChain.

// EmulatorPool fans one tool family out over an ordered emulator list
// (ADR-010). It implements Emulator: the first successful emulator answers;
// every failure moves to the next; all-failed surfaces the LAST error (the
// executor wraps it into the documented protocol-conform error result —
// behavior unchanged when the pool holds a single emulator).
type EmulatorPool struct {
	mu     sync.RWMutex
	search []Emulator
	fetch  []Emulator
}

// NewEmulatorPool builds a pool from per-family lists. Entries may be nil
// only when both lists are empty (the no-emulator degrade state).
func NewEmulatorPool(search, fetch []Emulator) *EmulatorPool {
	return &EmulatorPool{search: search, fetch: fetch}
}

// SetSearch atomically replaces the web_search list (live-swap contract:
// the provider reload rebuilds the pool without restart; in-flight calls
// keep their snapshot).
func (p *EmulatorPool) SetSearch(list []Emulator) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.search = list
}

// SetFetch atomically replaces the web_fetch list (same contract).
func (p *EmulatorPool) SetFetch(list []Emulator) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fetch = list
}

// SearchOrder returns a snapshot of the web_search list (the wiring test
// asserts the stored web_search_fallback KV order is what the pool
// walks — ADR-010 decision 3 is a runtime contract now).
func (p *EmulatorPool) SearchOrder() []Emulator {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]Emulator(nil), p.search...)
}

// FetchOrder returns a snapshot of the web_fetch list (same contract).
func (p *EmulatorPool) FetchOrder() []Emulator {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]Emulator(nil), p.fetch...)
}

// Search answers one web_search invocation, walking the family list.
func (p *EmulatorPool) Search(ctx context.Context, query string, st ir.ServerToolConfig) ([]ir.WebSearchResultItem, error) {
	p.mu.RLock()
	list := p.search
	p.mu.RUnlock()
	var out []ir.WebSearchResultItem
	err := walkPool(list, func(e Emulator) error {
		var err error
		out, err = e.Search(ctx, query, st)
		return err
	})
	return out, err
}

// Fetch answers one web_fetch invocation, walking the family list.
func (p *EmulatorPool) Fetch(ctx context.Context, url string, st ir.ServerToolConfig) (*ir.WebFetchContent, error) {
	p.mu.RLock()
	list := p.fetch
	p.mu.RUnlock()
	var out *ir.WebFetchContent
	err := walkPool(list, func(e Emulator) error {
		var err error
		out, err = e.Fetch(ctx, url, st)
		return err
	})
	return out, err
}

// walkPool tries each emulator in order; the first nil error wins. All
// failed → the last attempt's error (nil list → nil error, the no-emulator
// degrade state the executor's availability check covers).
func walkPool(list []Emulator, try func(Emulator) error) error {
	var last error
	for _, e := range list {
		if e == nil {
			continue
		}
		if err := try(e); err == nil {
			return nil
		} else {
			last = err
		}
	}
	return last
}
