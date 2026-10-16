// Package provider defines the Provider interface and the registry through
// which routing resolves provider implementations. It carries the provider
// contracts (Complete/Stream) and the concrete providers (Codex OAuth,
// Ollama Cloud, custom OpenAI-compatible).
package provider

import (
	"context"
	"time"

	"github.com/tubeof/proxyllm/internal/translate/ir"
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
	// upstream. Callers own the retry/failover decision —
	// Complete reports the attempt's terminal outcome only.
	//
	// Errors of type *UpstreamError carry the two-tier classification
	// inputs (HTTP status + body error code); other error types are
	// transport failures (connection refused, TLS reset, timeout), which
	// routing classifies as early-stream retryable when nothing has been
	// streamed yet.
	Complete(ctx context.Context, req ir.Request) (ir.Response, error)

	// Stream starts one streaming request against the upstream and
	// returns the canonical event channel. Events must be
	// drained until closed or cancel must be called — cancel aborts the
	// upstream request and releases its resources (client disconnect →
	// upstream cancellation).
	//
	// The returned error covers dispatch failures only (connection setup,
	// pre-stream HTTP errors). Failures after the first streamed event are
	// terminal: the channel delivers an ir.Error event and closes —
	// routing never retries or swaps mid-stream.
	Stream(ctx context.Context, req ir.Request) (<-chan ir.StreamEvent, func(), error)

	// RefreshCredentials refreshes OAuth tokens / API credentials when
	// applicable. No-op for static-key providers.
	RefreshCredentials(ctx context.Context) error
}

// UpstreamError is the typed upstream/API error every provider returns from
// Complete (and embeds in Stream dispatch failures): the two classification
// tiers routing needs — the HTTP status of the response and the error object
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
	// honors it for same-provider retries, capped by the backoff
	// ceiling.
	RetryAfter time.Duration

	// Message is the human-readable error message (redacted upstream —
	// credentials never ride errors).
	Message string
}

// Error implements error. The wire fields ride the message so logs and
// route_trace can record error_code + failure classification from
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

	// Decision indicates the decision surface (System One / Jev and the
	// OpenAI Decisions API): typed questions scored against shared
	// evidence in one non-streaming pass. Routing filters decision
	// requests on this flag; the provider must also implement Decider
	// for it to mean anything.
	Decision bool
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

// TypedProvider is the optional account-type surface a provider exposes
// (ADR-009): a provider row IS an account, and its type groups the
// accounts the routing engine pools under a chain's type entries. Every
// storage-backed provider (custom, ollamacloud, codex) implements this;
// test stubs may not — a provider without it never matches a type entry.
type TypedProvider interface {
	// Type returns the canonical provider type string (e.g.
	// "ollama-cloud", "codex-oauth") — the same value as the providers
	// table's type column the chain entries name.
	Type() string
}

// CredentialedProvider is the optional credential-presence surface
// (ADR-009 decision 4): accounts without a credential (abandoned OAuth
// drafts) are excluded from type expansion — routing live traffic to a
// guaranteed auth failure serves nobody. Providers without this surface
// are treated as credentialed (statically-keyed or keyless providers
// where "no key" is a legal routing state, mirroring the construction
// path's providerAPIKey contract).
type CredentialedProvider interface {
	// HasCredential reports whether the account can authenticate: an API
	// key or OAuth token is present.
	HasCredential() bool
}

// ModelInfo is one upstream model as the provider's discovery endpoint
// reports it (ADR-010 model sync). Name is the wire model identifier — the
// exact string a chat request must carry for that model.
type ModelInfo struct {
	// Name is the upstream model identifier (OpenAI "id", Ollama "name").
	Name string

	// ContextLength is the model's context window in tokens, when the
	// discovery endpoint reports one; 0 = undetected (Ollama Cloud's
	// /api/show model_info context_length is the source consulted).
	ContextLength int

	// Thinking / Tools / Vision are the model's detected capabilities;
	// false = undetected or absent.
	Thinking bool
	Tools    bool
	Vision   bool
}

// ModelLister is the optional model-discovery surface a provider exposes
// (ADR-010 model sync): providers implementing it participate in the
// automatic model import. Providers without it are simply never synced —
// existing implementations and test stubs do not change.
type ModelLister interface {
	// ListModels fetches the upstream model list. Errors are per-provider
	// (transport, auth, unsupported endpoint) — the syncer skips the
	// provider and records the reason; it never fails the whole run.
	ListModels(ctx context.Context) ([]ModelInfo, error)
}

// Decider is the optional decision surface (decision models — System One /
// Jev, the OpenAI Decisions API): a provider implementing it can answer IR
// decision requests. Providers without it never serve a decision route —
// the router's capability filter gates on Capabilities.Decision and the
// executor's decision walk skips implementations that do not satisfy this
// interface.
type Decider interface {
	// Decide performs one non-streaming decision round trip. Errors are
	// classified like any upstream failure (UpstreamError status/body
	// tiers); there is no streaming variant.
	Decide(ctx context.Context, req ir.DecisionRequest) (ir.DecisionResponse, error)
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
