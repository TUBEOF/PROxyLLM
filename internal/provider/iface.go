// Package provider defines the Provider interface and the registry through
// which routing resolves provider implementations. Phase 0 shipped the
// contracts + stubs; Phase 1 adds Complete/Stream (plan §4.2) and the
// concrete providers (Codex OAuth, Ollama Cloud, custom OpenAI-compatible).
package provider

import (
	"context"
	"time"

	"github.com/proxyllm/proxyllm/internal/translate/ir"
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

	// Complete performs one non-streaming request round trip against the
	// upstream (plan §4.2). Callers own the retry/failover decision —
	// Complete reports the attempt's terminal outcome only (FO-5).
	//
	// Errors of type *UpstreamError carry the two-tier classification
	// inputs (HTTP status + body error code, FO-2); other error types are
	// transport failures (connection refused, TLS reset, timeout), which
	// routing classifies as early-stream retryable when nothing has been
	// streamed yet (FO-1).
	Complete(ctx context.Context, req ir.Request) (ir.Response, error)

	// Stream starts one streaming request against the upstream and
	// returns the canonical event channel (plan §4.2). Events must be
	// drained until closed or cancel must be called — cancel aborts the
	// upstream request and releases its resources (client disconnect →
	// upstream cancellation, C2).
	//
	// The returned error covers dispatch failures only (connection setup,
	// pre-stream HTTP errors). Failures after the first streamed event are
	// terminal: the channel delivers an ir.Error event and closes —
	// routing never retries or swaps mid-stream (FO-1).
	Stream(ctx context.Context, req ir.Request) (<-chan ir.StreamEvent, func(), error)

	// RefreshCredentials refreshes OAuth tokens / API credentials when
	// applicable. No-op for static-key providers.
	RefreshCredentials(ctx context.Context) error
}

// UpstreamError is the typed upstream/API error every provider returns from
// Complete (and embeds in Stream dispatch failures): the two classification
// tiers FO-2 needs — the HTTP status of the response and the error object
// from the decoded body. Code/Type are the wire error.code/error.type; an
// empty HTTPStatus marks a failure that never produced a response
// (transport error).
type UpstreamError struct {
	// HTTPStatus is the HTTP status of the upstream response; 0 when the
	// request failed before a response arrived (transport error).
	HTTPStatus int

	// Type and Code are the decoded body error.type / error.code ("" when
	// the body carries no error object or failed to decode).
	Type string
	Code string

	// RetryAfter is the server-advertised retry delay (seconds) from a
	// Retry-After response header or body field, 0 when absent. Routing
	// honors it for same-provider retries, capped by the FO-3 backoff
	// ceiling (O10).
	RetryAfter time.Duration

	// Message is the human-readable error message (redacted upstream —
	// credentials never ride errors).
	Message string
}

// Error implements error. The wire fields ride the message so logs and
// route_trace can record error_code + failure classification (FO-4) from
// the typed fields, not by parsing strings.
func (e *UpstreamError) Error() string {
	if e.Message == "" {
		return "upstream error"
	}
	return e.Message
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
