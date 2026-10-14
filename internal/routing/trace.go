package routing

import (
	"strconv"
	"sync"
	"time"

	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// Trace is the route_trace of one request: every transition observable —
// original + fallback provider, status + body error code, failure position,
// tokens consumed, per-attempt and total latency. It serializes to the JSON
// stored in request_logs.route_trace.
//
// All methods are safe for concurrent use: the failover loop and the
// observability writer run on different goroutines once a stream is live.
type Trace struct {
	mu     sync.Mutex
	events []Event

	// payloads are the in-memory-only per-sub-operation payloads: what a
	// failover/emulation/caption step actually sent and received. They ride the
	// Trace because the Trace is what already reaches the log writer, but they
	// are deliberately NOT serialized: route_trace is a versioned wire schema
	// the WebUI renders, and bodies do not belong in it. Payloads() is how the
	// write path collects them.
	payloads []ChildPayload
}

// Payloads returns the collected sub-operation payloads in collection order.
// A copy, so a caller on another goroutine (the log writer) cannot race the
// routing goroutines still appending.
func (t *Trace) Payloads() []ChildPayload {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]ChildPayload, len(t.payloads))
	copy(out, t.payloads)
	return out
}

// EventType enumerates observable routing transitions (fallbacks are never
// silent — each carries its reason).
type EventType string

const (
	// EventAlias: the requested model name resolved to a configured
	// primary (alias resolution).
	EventAlias EventType = "alias"

	// EventSkipped: a chain entry was skipped (capability gap, health
	// cooldown, provider not registered) — or, for a capability fallback,
	// because its declared context window cannot hold the request
	// (ADR-013).
	EventSkipped EventType = "skipped"

	// EventAttempt: an upstream attempt started.
	EventAttempt EventType = "attempt"

	// EventFailed: an attempt failed with its classification (class,
	// error code, HTTP status, position).
	EventFailed EventType = "failed"

	// EventFailover: the request moved to the next chain entry.
	EventFailover EventType = "failover"

	// EventRetry: the request re-attempted the same provider (backoff).
	EventRetry EventType = "retry"

	// EventCompatPolicy: a per-model compat policy was applied (thinking
	// strip/map/block, clamps, emulation) — never silent.
	EventCompatPolicy EventType = "compat_policy"

	// EventExpand: a chain type entry expanded to the accounts of that
	// type (ADR-009) — From carries the type, To the comma-joined
	// account IDs. Expansions are never silent.
	EventExpand EventType = "expand"

	// EventRotation: the round-robin strategy rotated the candidate list
	// (ADR-009) — From/To carry the old/new first entry. Never silent.
	EventRotation EventType = "rotation"

	// EventEmulatedTool: a server tool was served by the emulation layer
	// (model tool call answered via the Firecrawl fallback) —
	// substitutions are never silent.
	EventEmulatedTool EventType = "emulated_tool"

	// EventEmulatedToolAttempt: one ACCOUNT's outcome inside an emulated tool
	// invocation — ProviderID names the account, Reason the
	// outcome (answered / empty / failed). The account walk is otherwise
	// invisible: without this the log can say a web tool ran but not why it
	// found nothing.
	EventEmulatedToolAttempt EventType = "emulated_tool_attempt"

	// EventRejected: the request ended without an upstream attempt (no
	// route, key scope, or empty chain after filtering).
	EventRejected EventType = "rejected"

	// EventCapabilityFallback: the rule chain had no provider for a
	// required capability, so the configured capability fallback chain
	// (ADR-010) was consulted — Capability names the family (e.g.
	// "vision"). Consultations are never silent.
	EventCapabilityFallback EventType = "capability_fallback"

	// EventImageCaption: a vision fallback model described an image and the
	// description replaced it in the request (ADR-014). Model/ProviderID name
	// the model that produced the description, LatencyMS the call, and Reason
	// carries its length — the operator must be able to see what the
	// answering model was told the image showed.
	EventImageCaption EventType = "image_caption"

	// EventModelResolved: the requested model ID (a prefixed routing
	// identity, e.g. "ollamacloud/glm-5.3") resolved to the upstream wire
	// name for dispatch (e.g. "glm-5.3") — From/To carry routing ID → wire
	// name. Never silent.
	EventModelResolved EventType = "model_resolved"

	// EventImplicitRoute: no rule matched, but the models-table row for
	// the model named a provider — the request routes there as a
	// one-entry chain (ADR-011). From carries the model ID, To the
	// provider. Implicit routes are never silent.
	EventImplicitRoute EventType = "implicit_route"

	// EventContextOverride: the requested model name carried the Claude
	// Code context-window marker ("[1m]"/"[1M]") and was stripped to the
	// base model before routing. From carries the typed name, To the stripped
	// base. The marker is a client-side context hint, never part of the model
	// identity — overrides are never silent.
	EventContextOverride EventType = "context_override"
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

	// Class + ErrorCode + HTTPStatus record the classification of a
	// failed attempt: the audit trail needs the body error code,
	// not just the status.
	Class      Class  `json:"class,omitempty"`
	ErrorCode  string `json:"error_code,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`

	// FailurePosition is pre_stream/mid_stream.
	FailurePosition FailurePosition `json:"failure_position,omitempty"`

	// Capability names the capability family of a capability_fallback
	// event (ADR-010).
	Capability string `json:"capability,omitempty"`

	// LatencyMS is the attempt duration (per-attempt latency).
	LatencyMS int64 `json:"latency_ms,omitempty"`

	// Usage is the token spend of the call an event describes, when it
	// describes one (a described image, an answered tool call). It rides the
	// event because the event is what the log writer turns into a sub-operation
	// row, and that row must be able to state what the step cost.
	Usage *ir.Usage `json:"usage,omitempty"`

	// CacheHit marks an image_caption whose description came from the caption
	// cache: the image was replaced, but no describer was called. Such an event
	// is a substitution, not an operation — it carries no usage and no latency,
	// and it produces no sub-operation row.
	CacheHit bool `json:"cache_hit,omitempty"`

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

// ModelResolved records the routing-ID → wire-name resolution: the chain
// dispatches the wire name, the client-facing surfaces echo the routing ID.
func (t *Trace) ModelResolved(routingID, wireName string) {
	t.add(Event{Type: EventModelResolved, From: routingID, To: wireName})
}

// ImplicitRoute records an implicit model route (ADR-011): no rule
// matched, the models-table row named the provider.
func (t *Trace) ImplicitRoute(modelID, providerID string) {
	t.add(Event{Type: EventImplicitRoute, From: modelID, To: providerID})
}

// ContextOverride records a stripped Claude Code context-window marker:
// the client typed name, routing continues with the base model.
func (t *Trace) ContextOverride(from, to string) {
	t.add(Event{Type: EventContextOverride, From: from, To: to})
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

// CompatPolicy records a compat-policy application (never silent).
func (t *Trace) CompatPolicy(setting, value, reason string) {
	t.add(Event{Type: EventCompatPolicy, Reason: reason,
		From: setting, To: value})
}

// CapabilityFallback records a capability-fallback consultation (ADR-010): the
// chain could not serve the capability named by capability, and the configured
// fallback models were tried.
func (t *Trace) CapabilityFallback(capability, reason string) {
	t.add(Event{Type: EventCapabilityFallback, Capability: capability, Reason: reason})
}

// ImageCaption records that a vision model described an image and the
// description was substituted into the request (ADR-014, never silent).
// latencyMS is the describer call, usage its token spend — the sub-operation
// row the log derives from this event reports what the step cost.
func (t *Trace) ImageCaption(providerID, model string, chars int, latencyMS int64, usage ir.Usage) {
	spent := usage
	t.add(Event{Type: EventImageCaption, ProviderID: providerID, Model: model,
		LatencyMS: latencyMS, Usage: &spent,
		Reason: strconv.Itoa(chars) + " character(s) described"})
}

// ImageCaptionCached records that an image was replaced by a description the
// cache already held. The image bytes were seen before, so no describer was
// called: the event names the model that produced the text originally, carries
// no spend, and — because nothing went upstream — produces no log row.
func (t *Trace) ImageCaptionCached(providerID, model string, chars int) {
	t.add(Event{Type: EventImageCaption, ProviderID: providerID, Model: model,
		CacheHit: true,
		Reason:   strconv.Itoa(chars) + " character(s) reused from the caption cache"})
}

// EmulatedTool records a server-tool substitution by the emulation
// layer (never silent). detail carries the invocation summary (tool,
// query/url, outcome); accountID names the emulating account that served the
// call (empty when none did), latencyMS the invocation.
func (t *Trace) EmulatedTool(toolName, accountID, detail string, latencyMS int64) {
	t.add(Event{Type: EventEmulatedTool, ProviderID: accountID, Reason: detail,
		To: toolName, LatencyMS: latencyMS})
}

// EmulatedToolAttempt records one account's outcome inside an emulated tool
// invocation: which account was asked, and whether it answered,
// came back empty, or failed. The account walk happens inside the pool, so this
// is the only place the operator can see WHY a web tool found nothing — and
// which account to look at when one of several is misconfigured.
func (t *Trace) EmulatedToolAttempt(toolName, providerID, outcome string) {
	t.add(Event{Type: EventEmulatedToolAttempt, To: toolName, ProviderID: providerID, Reason: outcome})
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
