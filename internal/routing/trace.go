package routing

import (
	"sync"
	"time"
)

// Trace is the route_trace of one request (plan Analysis E: every
// transition observable; FO-4: original + fallback provider, status + body
// error code, failure position, tokens consumed, per-attempt and total
// latency). It serializes to the JSON stored in request_logs.route_trace.
//
// All methods are safe for concurrent use: the failover loop and the
// observability writer run on different goroutines once a stream is live.
type Trace struct {
	mu     sync.Mutex
	events []Event
}

// EventType enumerates observable routing transitions (fallbacks are never
// silent — each carries its reason).
type EventType string

const (
	// EventAlias: the requested model name resolved to a configured
	// primary (C6 alias resolution).
	EventAlias EventType = "alias"

	// EventSkipped: a chain entry was skipped (capability gap, health
	// cooldown, provider not registered).
	EventSkipped EventType = "skipped"

	// EventAttempt: an upstream attempt started.
	EventAttempt EventType = "attempt"

	// EventFailed: an attempt failed with its classification (class,
	// error code, HTTP status, position — FO-2/FO-4).
	EventFailed EventType = "failed"

	// EventFailover: the request moved to the next chain entry.
	EventFailover EventType = "failover"

	// EventRetry: the request re-attempted the same provider (backoff).
	EventRetry EventType = "retry"

	// EventCompatPolicy: a per-model compat policy was applied (§6.4:
	// thinking strip/map/block, clamps, emulation) — never silent.
	EventCompatPolicy EventType = "compat_policy"

	// EventExpand: a chain type entry expanded to the accounts of that
	// type (ADR-009) — From carries the type, To the comma-joined
	// account IDs. Expansions are never silent.
	EventExpand EventType = "expand"

	// EventRotation: the round-robin strategy rotated the candidate list
	// (ADR-009) — From/To carry the old/new first entry. Never silent.
	EventRotation EventType = "rotation"

	// EventEmulatedTool: a server tool was served by the §6.2 emulation
	// layer (model tool call answered via the Firecrawl fallback) —
	// substitutions are never silent.
	EventEmulatedTool EventType = "emulated_tool"

	// EventRejected: the request ended without an upstream attempt (no
	// route, key scope, or empty chain after filtering).
	EventRejected EventType = "rejected"

	// EventCapabilityFallback: the rule chain had no provider for a
	// required capability, so the configured capability fallback chain
	// (ADR-010) was consulted — Capability names the family (e.g.
	// "vision"). Consultations are never silent.
	EventCapabilityFallback EventType = "capability_fallback"

	// EventModelResolved: the requested model ID (a prefixed routing
	// identity, e.g. "ollamacloud/glm-5.3") resolved to the upstream wire
	// name for dispatch (e.g. "glm-5.3") — From/To carry routing ID → wire
	// name (2026-09-21 model identity). Never silent.
	EventModelResolved EventType = "model_resolved"

	// EventImplicitRoute: no rule matched, but the models-table row for
	// the model named a provider — the request routes there as a
	// one-entry chain (ADR-011). From carries the model ID, To the
	// provider. Implicit routes are never silent.
	EventImplicitRoute EventType = "implicit_route"
)

// Event is one observable routing transition.
type Event struct {
	Type   EventType `json:"type"`
	Reason string    `json:"reason,omitempty"`

	// ProviderID is the provider this transition concerns.
	ProviderID string `json:"provider_id,omitempty"`

	// Model names the model involved (resolved, post-alias).
	Model string `json:"model,omitempty"`

	// From/To carry alias and failover transitions.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`

	// Class + ErrorCode + HTTPStatus record the FO-2 classification of a
	// failed attempt (FO-4: the audit trail needs the body error code,
	// not just the status).
	Class      Class  `json:"class,omitempty"`
	ErrorCode  string `json:"error_code,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`

	// FailurePosition is pre_stream/mid_stream (FO-4).
	FailurePosition FailurePosition `json:"failure_position,omitempty"`

	// Capability names the capability family of a capability_fallback
	// event (ADR-010).
	Capability string `json:"capability,omitempty"`

	// LatencyMS is the attempt duration (per-attempt latency, FO-4).
	LatencyMS int64 `json:"latency_ms,omitempty"`

	// ts is the event time (not serialized per event; the trace carries
	// relative ordering, and events are appended in order).
	ts time.Time
}

// add appends one event (internal; callers use the typed recorders below).
func (t *Trace) add(ev Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ev.ts = time.Now()
	t.events = append(t.events, ev)
}

// Alias records an alias resolution.
func (t *Trace) Alias(from, to string) {
	t.add(Event{Type: EventAlias, From: from, To: to})
}

// ModelResolved records the routing-ID → wire-name resolution (2026-09-21
// model identity): the chain dispatches the wire name, the client-facing
// surfaces echo the routing ID.
func (t *Trace) ModelResolved(routingID, wireName string) {
	t.add(Event{Type: EventModelResolved, From: routingID, To: wireName})
}

// ImplicitRoute records an implicit model route (ADR-011): no rule
// matched, the models-table row named the provider.
func (t *Trace) ImplicitRoute(modelID, providerID string) {
	t.add(Event{Type: EventImplicitRoute, From: modelID, To: providerID})
}

// Skip records a skipped chain entry.
func (t *Trace) Skip(providerID, reason string) {
	t.add(Event{Type: EventSkipped, ProviderID: providerID, Reason: reason})
}

// Attempt records an upstream attempt start.
func (t *Trace) Attempt(providerID, model string) {
	t.add(Event{Type: EventAttempt, ProviderID: providerID, Model: model})
}

// Failed records a failed attempt with its classification.
func (t *Trace) Failed(providerID string, c Classification, status int, errorCode string, latencyMS int64) {
	t.add(Event{Type: EventFailed, ProviderID: providerID,
		Class: c.Class, FailurePosition: c.FailurePosition,
		HTTPStatus: status, ErrorCode: errorCode, LatencyMS: latencyMS})
}

// Failover records a move to the next chain entry.
func (t *Trace) Failover(from, to string, reason string) {
	t.add(Event{Type: EventFailover, From: from, To: to, Reason: reason})
}

// Retry records a same-provider retry.
func (t *Trace) Retry(providerID string, reason string) {
	t.add(Event{Type: EventRetry, ProviderID: providerID, Reason: reason})
}

// CompatPolicy records a compat-policy application (§6.4, never silent).
func (t *Trace) CompatPolicy(setting, value, reason string) {
	t.add(Event{Type: EventCompatPolicy, Reason: reason,
		From: setting, To: value})
}

// CapabilityFallback records a capability-fallback consultation (ADR-010 /
// 2026-09-21 model fallback): the chain could not serve the capability
// named by capability, and the configured fallback models were tried.
func (t *Trace) CapabilityFallback(capability, reason string) {
	t.add(Event{Type: EventCapabilityFallback, Capability: capability, Reason: reason})
}

// EmulatedTool records a server-tool substitution by the §6.2 emulation
// layer (never silent). detail carries the invocation summary (tool,
// query/url, outcome).
func (t *Trace) EmulatedTool(toolName, detail string) {
	t.add(Event{Type: EventEmulatedTool, Reason: detail, To: toolName})
}

// Reject records a terminal no-upstream outcome.
func (t *Trace) Reject(reason string) {
	t.add(Event{Type: EventRejected, Reason: reason})
}

// Events returns a copy of the recorded events, in order.
func (t *Trace) Events() []Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Event, len(t.events))
	copy(out, t.events)
	return out
}
