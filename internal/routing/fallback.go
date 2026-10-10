package routing

import (
	"context"
	"strings"
	"sync"
	"time"

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

// EmulatorEntry is one entry of a family list: the account's id and its
// Emulator. The id is the provider row id, so it survives a live swap — a
// circuit keyed by POSITION would forget a cooling account the moment the
// operator reordered the list, and the trace could not name the account at all.
type EmulatorEntry struct {
	ID       string
	Emulator Emulator
}

// Per-account circuit (2026-10-10): a misconfigured or down account used to be
// retried first on EVERY call, paying its failure (up to the call budget) before
// the walk moved on. Three consecutive hard failures cool it down; a success
// resets it. A cooling account is skipped unless every account is cooling, in
// which case the list is walked anyway as a probe — the pool must never become
// unusable because the whole family had a bad minute.
const (
	emuFailureThreshold = 3
	emuCooldownBase     = 30 * time.Second
	emuCooldownCeiling  = 5 * time.Minute
)

// emuCircuit is one account's circuit state.
type emuCircuit struct {
	failures int
	until    time.Time
}

// EmulatorPool fans one tool family out over an ordered emulator list
// (ADR-010). It implements Emulator: the first successful emulator answers;
// every failure moves to the next; all-failed surfaces the LAST error (the
// executor wraps it into the documented protocol-conform error result —
// behavior unchanged when the pool holds a single emulator).
//
// An EMPTY answer is a success (2026-10-10) — see walk — and the walk skips
// accounts in cooldown.
type EmulatorPool struct {
	mu     sync.RWMutex
	search []EmulatorEntry
	fetch  []EmulatorEntry

	// circuits is per-account state, keyed by account id. Its own lock: the
	// list lock is only held to read the entry lists.
	circuitsMu sync.Mutex
	circuits   map[string]*emuCircuit

	// clock is injectable so the cooldown tests need no real waiting.
	clock func() time.Time
}

// NewEmulatorPool builds a pool from per-family lists of unidentified emulators
// (no account id: no circuit, and the trace cannot name the account). Kept for
// callers that have no identity — tests, and wiring that never had one.
func NewEmulatorPool(search, fetch []Emulator) *EmulatorPool {
	return NewEmulatorPoolEntries(anonymous(search), anonymous(fetch))
}

func anonymous(list []Emulator) []EmulatorEntry {
	out := make([]EmulatorEntry, 0, len(list))
	for _, e := range list {
		out = append(out, EmulatorEntry{Emulator: e})
	}
	return out
}

// NewEmulatorPoolEntries builds a pool from identified entries. The account id
// is what the trace names and what the per-account circuit is keyed by.
func NewEmulatorPoolEntries(search, fetch []EmulatorEntry) *EmulatorPool {
	return &EmulatorPool{
		search:   search,
		fetch:    fetch,
		circuits: map[string]*emuCircuit{},
		clock:    time.Now,
	}
}

// SetSearch atomically replaces the web_search list (live-swap contract:
// the provider reload rebuilds the pool without restart; in-flight calls
// keep their snapshot).
func (p *EmulatorPool) SetSearch(list []Emulator) {
	p.SetSearchEntries(anonymous(list))
}

// SetFetch atomically replaces the web_fetch list (same contract).
func (p *EmulatorPool) SetFetch(list []Emulator) {
	p.SetFetchEntries(anonymous(list))
}

// SetSearchEntries atomically replaces the web_search list with identified
// entries (the wiring's live-swap path).
func (p *EmulatorPool) SetSearchEntries(list []EmulatorEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.search = list
}

// SetFetchEntries atomically replaces the web_fetch list (same contract).
func (p *EmulatorPool) SetFetchEntries(list []EmulatorEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fetch = list
}

// SearchOrder returns a snapshot of the web_search list (the wiring test
// asserts the stored web_search_fallback KV order is what the pool
// walks — ADR-010 decision 3 is a runtime contract now).
func (p *EmulatorPool) SearchOrder() []Emulator {
	return emulators(p.searchSnapshot())
}

// FetchOrder returns a snapshot of the web_fetch list (same contract).
func (p *EmulatorPool) FetchOrder() []Emulator {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return emulators(p.fetch)
}

func (p *EmulatorPool) searchSnapshot() []EmulatorEntry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]EmulatorEntry(nil), p.search...)
}

func emulators(list []EmulatorEntry) []Emulator {
	out := make([]Emulator, 0, len(list))
	for _, e := range list {
		out = append(out, e.Emulator)
	}
	return out
}

// Search answers one web_search invocation, walking the family list.
func (p *EmulatorPool) Search(ctx context.Context, query string, st ir.ServerToolConfig) ([]ir.WebSearchResultItem, error) {
	out, _, err := p.SearchTraced(ctx, query, st, nil)
	return out, err
}

// SearchTraced answers one web_search invocation and records the account walk
// in the trace, reporting which account answered. It is a concrete method (not
// part of the Emulator interface) so the executor can pass the trace when the
// configured emulator is a pool; a single emulator has no walk to report and
// keeps using the plain method.
func (p *EmulatorPool) SearchTraced(ctx context.Context, query string, st ir.ServerToolConfig, trace *Trace) ([]ir.WebSearchResultItem, string, error) {
	list := p.searchSnapshot()
	var out []ir.WebSearchResultItem
	by, err := p.walk(list, trace, "web_search", func(e Emulator) (bool, error) {
		found, err := e.Search(ctx, query, st)
		if err != nil {
			// Leave out as it is: a failed attempt has no answer to offer.
			return false, err
		}
		out = found
		// An empty result set is a successful answer with nothing in it.
		return len(found) == 0, nil
	})
	return out, by, err
}

// Fetch answers one web_fetch invocation, walking the family list.
func (p *EmulatorPool) Fetch(ctx context.Context, url string, st ir.ServerToolConfig) (*ir.WebFetchContent, error) {
	out, _, err := p.FetchTraced(ctx, url, st, nil)
	return out, err
}

// FetchTraced is Fetch with the account walk recorded in the trace.
func (p *EmulatorPool) FetchTraced(ctx context.Context, url string, st ir.ServerToolConfig, trace *Trace) (*ir.WebFetchContent, string, error) {
	p.mu.RLock()
	list := append([]EmulatorEntry(nil), p.fetch...)
	p.mu.RUnlock()
	var out *ir.WebFetchContent
	by, err := p.walk(list, trace, "web_fetch", func(e Emulator) (bool, error) {
		page, err := e.Fetch(ctx, url, st)
		if err != nil {
			return false, err
		}
		out = page
		// A page with no content — including one Firecrawl answered 200 for but
		// could not serve — is empty, not an answer.
		return page == nil || strings.TrimSpace(page.Data) == "", nil
	})
	return out, by, err
}

// walk tries each account in order with a three-way outcome (2026-10-10):
//
//   - a NON-EMPTY success returns immediately — an answer that settles it,
//   - an EMPTY success is remembered and the walk continues, because another
//     account may answer where this one found nothing,
//   - a failure moves on, its error kept only in case nothing answers at all.
//
// An empty answer therefore outranks a later error: the request earned
// "nothing found", and reporting another account's failure instead would be
// wrong (and would tell the model the tool is broken when it is not). Only when
// every account failed is the LAST error surfaced — the pre-existing contract.
//
// try reports emptiness so the pool decides what empty means per family (an
// empty result set, a page with no content); the Emulator interface stays two
// methods, so every existing implementation and fake is unaffected.
//
// It returns the id of the account that ANSWERED (non-empty), for the caller's
// trace detail: "" when none did.
func (p *EmulatorPool) walk(list []EmulatorEntry, trace *Trace, tool string, try func(Emulator) (empty bool, err error)) (string, error) {
	var last error
	answeredEmpty := false
	answeredBy := ""

	for _, e := range p.attemptOrder(list) {
		if e.Emulator == nil {
			continue
		}
		empty, err := try(e.Emulator)
		if err != nil {
			p.reportFailure(e.ID)
			p.noteAttempt(trace, tool, e.ID, "failed")
			last = err
			continue
		}
		p.reportSuccess(e.ID)
		if !empty {
			p.noteAttempt(trace, tool, e.ID, "answered")
			answeredBy = e.ID
			return answeredBy, nil
		}
		p.noteAttempt(trace, tool, e.ID, "empty")
		answeredEmpty = true
	}
	if answeredEmpty {
		return "", nil
	}
	return "", last
}

// attemptOrder returns the accounts to try, in list order, with cooling
// accounts skipped — unless every identified account is cooling, in which case
// the whole list is returned as a probe (a family that had a bad minute must
// recover by itself). Unidentified entries are never gated: there is nothing to
// key a circuit by.
func (p *EmulatorPool) attemptOrder(list []EmulatorEntry) []EmulatorEntry {
	now := p.clock()
	healthy := make([]EmulatorEntry, 0, len(list))
	cooling := 0
	identified := 0
	for _, e := range list {
		if e.ID == "" || p.healthy(e.ID, now) {
			healthy = append(healthy, e)
			continue
		}
		cooling++
		if e.ID != "" {
			identified++
		}
	}
	if len(healthy) == 0 && cooling > 0 {
		return list // every account is cooling: probe them all, in order
	}
	return healthy
}

// healthy reports whether an account may be tried right now.
func (p *EmulatorPool) healthy(id string, now time.Time) bool {
	p.circuitsMu.Lock()
	defer p.circuitsMu.Unlock()
	c := p.circuits[id]
	return c == nil || !now.Before(c.until)
}

// reportFailure counts a hard failure and cools the account down once it has
// failed emuFailureThreshold times in a row, doubling the window up to the
// ceiling.
func (p *EmulatorPool) reportFailure(id string) {
	if id == "" {
		return
	}
	p.circuitsMu.Lock()
	defer p.circuitsMu.Unlock()
	c := p.circuits[id]
	if c == nil {
		c = &emuCircuit{}
		p.circuits[id] = c
	}
	c.failures++
	if c.failures < emuFailureThreshold {
		return
	}
	cooldown := emuCooldownBase
	for i := emuFailureThreshold; i < c.failures && cooldown < emuCooldownCeiling; i++ {
		cooldown *= 2
	}
	if cooldown > emuCooldownCeiling {
		cooldown = emuCooldownCeiling
	}
	c.until = p.clock().Add(cooldown)
}

// reportSuccess clears an account's circuit: an account that answers is
// healthy, whatever happened before.
func (p *EmulatorPool) reportSuccess(id string) {
	if id == "" {
		return
	}
	p.circuitsMu.Lock()
	defer p.circuitsMu.Unlock()
	delete(p.circuits, id)
}

// noteAttempt records one account's outcome in the trace, when there is one to
// record into.
func (p *EmulatorPool) noteAttempt(trace *Trace, tool, id, outcome string) {
	if trace == nil || id == "" {
		return
	}
	trace.EmulatedToolAttempt(tool, id, outcome)
}
