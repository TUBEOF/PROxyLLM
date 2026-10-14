package provider

// LiveRegistry is a Registry whose provider set can be replaced at runtime:
// the WebUI saves a provider, the admin API
// reloads the set from the store, and the next request resolves against
// the NEW set — without a restart. The static registry's panic-on-duplicate
// contract fits assembly-time construction, not live edits.
//
// Thread-safety: routing resolves concurrently with reloads; every access
// takes the mutex. Get/List/Register keep the static Registry semantics
// (Register panics on duplicate — still a programming error, the reload
// path goes through ReplaceDynamics).

import "sync"

// NewLiveRegistry builds an empty live registry. Static providers (from
// WithProvider) are added with Register; dynamic ones (from stored config)
// ride ReplaceDynamics.
func NewLiveRegistry() *LiveRegistry {
	return &LiveRegistry{
		providers: make(map[string]Provider),
	}
}

// LiveRegistry is the runtime-mutable Registry.
type LiveRegistry struct {
	mu sync.RWMutex

	// providers is the full set (static + dynamic). The order slice keeps
	// List() deterministic (insertion order).
	providers map[string]Provider
	order     []string

	// dynamic marks the provider IDs that came from stored config and are
	// therefore replaceable by ReplaceDynamics; static registrations are
	// never removed by a reload.
	dynamic map[string]bool
}

// Register adds a provider permanently (Registry contract). Registering a
// duplicate ID is a programming error and panics.
func (r *LiveRegistry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.providers[p.ID()]; ok {
		panic("provider: duplicate registration: " + p.ID())
	}
	r.providers[p.ID()] = p
	r.order = append(r.order, p.ID())
}

// ReplaceDynamics atomically swaps the dynamic (store-derived) provider
// set for the given providers. Static providers are untouched: a reload
// never removes a WithProvider-registered provider even when a stored row
// with the same ID exists (the caller must avoid such shadowing — the
// server logs it at startup).
//
// A full failed construction in the reload path is the caller's concern:
// this method only replaces, never merges — last-write-wins, matching the
// storage layer's own put semantics.
func (r *LiveRegistry) ReplaceDynamics(replacements []Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Drop all current dynamics.
	next := make(map[string]Provider, len(r.providers)+len(replacements))
	for id, p := range r.providers {
		if !r.dynamic[id] {
			next[id] = p
		}
	}
	nextOrder := make([]string, 0, len(r.order)+len(replacements))
	for _, id := range r.order {
		if !r.dynamic[id] {
			nextOrder = append(nextOrder, id)
		}
	}

	// Add the new set. A duplicate within the replacement list is a
	// programming error at the reload source (the store rejects duplicate
	// IDs), so it panics like Register.
	for _, p := range replacements {
		id := p.ID()
		if _, ok := next[id]; ok {
			panic("provider: duplicate registration: " + id)
		}
		next[id] = p
		nextOrder = append(nextOrder, id)
	}

	r.providers = next
	r.order = nextOrder

	dynamic := make(map[string]bool, len(replacements))
	for _, p := range replacements {
		dynamic[p.ID()] = true
	}
	r.dynamic = dynamic
}

// Get returns the provider registered under id.
func (r *LiveRegistry) Get(id string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[id]
	return p, ok
}

// List returns all registered providers in insertion order.
func (r *LiveRegistry) List() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Provider, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.providers[id])
	}
	return out
}
