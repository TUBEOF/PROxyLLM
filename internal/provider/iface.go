// Package provider defines the Provider interface and the registry through
// which routing resolves provider implementations. Phase 0 ships the
// contracts + stubs; concrete providers (Codex OAuth, Ollama Cloud,
// custom OpenAI-compatible) land in Phase 1.
package provider

import (
	"context"
)

// Provider is the contract every upstream integration implements. The
// registry maps provider IDs to implementations; adding a provider type
// must never require changes to routing or the API surfaces.
type Provider interface {
	// ID returns the stable, unique provider identifier.
	ID() string

	// Capabilities reports what this provider supports; routing filters
	// and failsover on this set.
	Capabilities() Capabilities

	// RefreshCredentials refreshes OAuth tokens / API credentials when
	// applicable. No-op for static-key providers.
	RefreshCredentials(ctx context.Context) error
}

// Capabilities is the capability set a provider reports to routing.
// Wire-protocol capabilities (streaming, tools) and tool abilities
// (vision, web_search, …) live here; per-model overrides live in the
// models table.
type Capabilities struct {
	// Streaming indicates SSE streaming support.
	Streaming bool

	// Tools indicates client-side tool/function calling support.
	Tools bool

	// Vision indicates image input support.
	Vision bool

	// WebSearch indicates native web search (server tool).
	WebSearch bool

	// WebFetch indicates native web fetch (server tool).
	WebFetch bool

	// CodeExecution indicates native code execution (server tool).
	CodeExecution bool

	// Thinking indicates extended-thinking support with signatures.
	Thinking bool
}

// Registry resolves provider IDs to implementations. Register is part of
// the interface so provider implementations can self-register from any
// package — adding a provider type must never require changes to routing
// or the API surfaces.
type Registry interface {
	// Get returns the provider registered under id.
	Get(id string) (Provider, bool)

	// List returns all registered providers.
	List() []Provider

	// Register adds a provider to the registry. Registering a duplicate
	// ID is a programming error and panics.
	Register(p Provider)
}

// NewRegistry returns the default registry with no providers registered.
func NewRegistry() Registry {
	return &registry{providers: make(map[string]Provider)}
}

type registry struct {
	providers map[string]Provider
	order     []string
}

// Register adds a provider to the registry. Registering a duplicate ID is
// a programming error and panics.
func (r *registry) Register(p Provider) {
	if _, ok := r.providers[p.ID()]; ok {
		panic("provider: duplicate registration: " + p.ID())
	}
	r.providers[p.ID()] = p
	r.order = append(r.order, p.ID())
}

func (r *registry) Get(id string) (Provider, bool) {
	p, ok := r.providers[id]
	return p, ok
}

func (r *registry) List() []Provider {
	out := make([]Provider, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.providers[id])
	}
	return out
}
