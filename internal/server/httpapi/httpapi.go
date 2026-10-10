// Package httpapi implements the client-facing API surfaces (plan §5.1):
// the OpenAI-compatible surface (/v1/chat/completions, /v1/responses,
// GET /v1/models) and the Anthropic-compatible surface (/v1/messages,
// /v1/messages/count_tokens, dual-shape model discovery). Requests decode
// into the canonical IR, route through the Router + Executor, and
// re-encode into the requesting protocol — same-surface relay and
// cross-surface translation share one pipeline (Analysis D hub-and-spoke).
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tubeof/proxyllm/internal/observability"
	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/reqlog"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// childRedactor masks credential shapes in sub-operation details. The full
// redactor (not just the narrow upstream-message mask) because the emulated
// tool path carries a model-supplied URL, which can embed a token in its
// query string — a shape urlTokenRe catches and the upstream mask does not.
var childRedactor = observability.NewRedactor()

// Params wires the API surfaces to their collaborators.
type Params struct {
	// Store persists models (compat policies) and request logs.
	Store *storage.Store

	// Router resolves fallback chains from the routing rules.
	Router *routing.Router

	// Executor dispatches requests through the chain with failover.
	Executor *routing.Executor

	// HasWebToolAccounts reports the web-tool account presence at
	// assembly time (R5 emulation default). nil keeps native; the server
	// refreshes it via RefreshWebToolAccounts on provider reloads.
	HasWebToolAccounts func() bool

	// Stream is the admin log stream hub (in-flight registry + finished-row
	// broadcast). nil keeps it disabled — every existing test constructs
	// api without it and stays green.
	Stream *reqlog.Stream
}

// api holds the wired collaborators.
type api struct {
	store    *storage.Store
	router   *routing.Router
	executor *routing.Executor
	stream   *reqlog.Stream // nil = live log stream disabled
	// hasWebToolAccounts is refreshed by the admin provider reload (the
	// emulation default consults it per compatFor; a per-request store
	// scan is the alternative — a bool swap avoids the hot-path query).
	hasWebToolAccounts atomic.Bool
}

// API is the assembled client-facing handler (New's return): the mux
// itself, with the refresh points the server's admin reload hooks need
// (R5 emulation-presence live swap).
type API struct {
	http.Handler
	a *api
}

// SetWebToolAccounts swaps the cached emulation-presence bool (the admin
// provider reload calls it). Atomic — the emulation default picks up the
// next request after a provider write.
func (p *API) SetWebToolAccounts(present bool) { p.a.hasWebToolAccounts.Store(present) }

// New assembles the client-facing HTTP handler (plan §5.1).
func New(p Params) *API {
	a := &api{store: p.Store, router: p.Router, executor: p.Executor, stream: p.Stream}
	if p.HasWebToolAccounts != nil {
		a.hasWebToolAccounts.Store(p.HasWebToolAccounts())
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", a.handleChat)
	mux.HandleFunc("POST /v1/responses", a.handleResponses)
	mux.HandleFunc("POST /v1/messages", a.handleMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", a.handleCountTokens)
	mux.HandleFunc("GET /v1/models", a.handleModels)
	return &API{Handler: mux, a: a}
}

// --- shared pipeline ---

// wireCompat is the JSON-tagged mirror of routing.Compat for decoding
// models.compat rows (routing.Compat deliberately has no JSON tags — the
// storage wire shape belongs to the HTTP layer, §6.4).
type wireCompat struct {
	ThinkingHandling    string `json:"thinking_handling"`
	ServerToolEmulation string `json:"server_tool_emulation"`
	MaxTokensClamp      int    `json:"max_tokens_clamp"`
	ContextWindow       int    `json:"context_window"`
	CacheControl        string `json:"cache_control"`
}

// compatFor loads the per-model compat policy. The model row's
// capabilities drive the policy now (2026-10-06, R5: the WebUI compat
// editor is REMOVED — the defaults are internal policy, capability-aware;
// the plan's R5 decision): a stored compat blob still wins when present
// (legacy rows keep their explicitly-set behavior — read-only, the SPA
// no longer writes this field).
//
// A model without a row (or with an unparsable compat blob) is not an
// error: it routes with the default (strip) policy — the store is
// eventually consistent, and requests must not hard-fail on a missing
// row.
func (a *api) compatFor(ctx context.Context, model string) routing.Compat {
	// Follow routing's context-marker strip: a suffixed request
	// ("glm-5.3-flash[1M]") carries no row of its own — the operator's
	// compat policy lives on the base row and must apply (otherwise the
	// marker would silently bypass thinking handling, clamps, emulation).
	base, _ := routing.StripContextMarker(model)
	var c routing.Compat
	m, err := a.store.GetModel(ctx, base)
	if err != nil {
		return c // zero value: strip default, no clamp
	}
	var w wireCompat
	if len(m.Compat) > 0 {
		_ = json.Unmarshal(m.Compat, &w)
	}
	c.ThinkingHandling = defaultThinkingFor(m.Capabilities, w.ThinkingHandling)
	c.ServerToolEmulation = emulationFor(a.hasWebToolAccounts.Load(), w.ServerToolEmulation)
	c.MaxTokensClamp = w.MaxTokensClamp
	c.ContextWindow = w.ContextWindow
	c.CacheControl = defaultCacheControl(w.CacheControl)
	return c
}

// defaultThinkingFor derives the thinking policy from the model row's
// capabilities when the stored blob left it unset (or was never written
// again): a thinking-capable target maps the budget to the reasoning
// effort it actually understands; everything else strips (signatures
// must never reach a non-thinking target — A12).
func defaultThinkingFor(capsJSON json.RawMessage, stored string) string {
	if stored != "" {
		return stored
	}
	for _, flag := range capabilityFlags(capsJSON) {
		if flag == "thinking" {
			return routing.ThinkingMapToEffort
		}
	}
	return "" // routing.Compat's zero value: the strip default (compat.go)
}

// capabilityFlags parses the model row's capability blob's true-valued
// flags; a corrupted blob reads as empty (never a routing failure).
func capabilityFlags(capsJSON json.RawMessage) []string {
	if len(capsJSON) == 0 {
		return nil
	}
	var flags map[string]bool
	if err := json.Unmarshal(capsJSON, &flags); err != nil {
		return nil
	}
	present := make([]string, 0, len(flags))
	for name, on := range flags {
		if on {
			present = append(present, name)
		}
	}
	return present
}

// defaultCacheControl pins the C4 rule as the enforced default: forward
// markers only Anthropic→Anthropic (the caller decides by the target's
// surface — for all other targets the stored value stays "" = the drop
// default through compat.go).
func defaultCacheControl(stored string) string {
	if stored == routing.CacheControlForward {
		return routing.CacheControlForward
	}
	return "" // compat.go: "" = drop for non-Anthropic targets
}

// emulationFor pins the R5 emulation default: emulate-firecrawl whenever
// a web-tool account exists (the model cannot serve native web search
// through this proxy without one), otherwise native (forward whatever
// the provider accepts). A stored blob value still wins (legacy rows).
func emulationFor(hasWebTool bool, stored string) string {
	if stored != "" {
		return stored
	}
	if hasWebTool {
		return routing.EmulationEmulateFirecrawl
	}
	return routing.EmulationNative
}

// Settings keys read by this package (the SPA Settings surface writes them
// into the same settings KV the server's DB override layer owns).
const (
	settingKeyNonClaude = "anthropic_surface_non_claude"
)

// Non-Claude gate values (§6.4 global switch, ADR-008).
const (
	gateAllow = "allow"
	gateWarn  = "warn"
	gateBlock = "block"
)

// ErrNonClaudeBlocked is the terminal error for the block gate.
var ErrNonClaudeBlocked = errors.New("httpapi: non-Claude models are blocked on the Anthropic surface")

// nonClaudeGate reads the global switch from the settings KV. Any missing,
// unreadable, or invalid value is allow — the safety net must never
// de-route on store noise (§6.4 default allow).
func (a *api) nonClaudeGate(ctx context.Context) string {
	raw, ok, err := a.store.GetConfigValue(ctx, settingKeyNonClaude)
	if err != nil || !ok {
		return gateAllow
	}
	switch raw {
	case gateAllow, gateWarn, gateBlock:
		return raw
	default:
		return gateAllow
	}
}

// isClaudeModel reports whether the requested model name is a Claude
// family model (the gate spares them). `claude-` is the Anthropic naming
// convention across the published catalog (§2.1); anything else on the
// Anthropic surface is a non-Claude target by OP-2 definition.
func isClaudeModel(model string) bool {
	return strings.HasPrefix(model, "claude-")
}

// applyNonClaudeGate enforces the global switch (§6.4): a non-Claude
// target on the Anthropic surface is annotated under warn (compat_policy
// event on the trace), refused under block, and passes untouched under
// allow. The trace records every applied policy — never silent.
func (a *api) applyNonClaudeGate(ctx context.Context, model string, trace *routing.Trace) error {
	switch a.nonClaudeGate(ctx) {
	case gateBlock:
		if !isClaudeModel(model) {
			trace.CompatPolicy(settingKeyNonClaude, gateBlock, "non-Claude target refused by the global Anthropic-surface switch")
			return ErrNonClaudeBlocked
		}
	case gateWarn:
		if !isClaudeModel(model) {
			trace.CompatPolicy(settingKeyNonClaude, gateWarn, "non-Claude target allowed with warning")
		}
	}
	return nil
}

// requiredCapabilities derives the capability set the request needs from
// its content: vision from image parts, tools from tool definitions,
// streaming, thinking, and web_search/web_fetch from server-tool defs
// (plan §6.4 / Analysis E capability filter; A8 family identity).
func requiredCapabilities(req ir.Request) provider.Capabilities {
	var caps provider.Capabilities
	if req.Stream {
		caps.Streaming = true
	}
	if len(req.Tools) > 0 {
		caps.Tools = true
	}
	if req.Thinking != nil && (req.Thinking.Enabled || req.Thinking.Adaptive) {
		caps.Thinking = true
	}
	for _, tool := range req.Tools {
		switch tool.ServerToolName() {
		case "web_search":
			caps.WebSearch = true
		case "web_fetch":
			caps.WebFetch = true
		}
	}
	for _, msg := range req.Messages {
		for _, part := range msg.Parts {
			if _, ok := part.(ir.ImagePart); ok {
				caps.Vision = true
			}
		}
	}
	return caps
}

// chainTrace is the JSON object stored in request_logs.route_trace:
// {"version":4,"events":[...]} with the compat, router, and executor
// events concatenated in order (plan §4.3 observability — an object, so
// the row can grow metadata without a breaking change). Version 2 adds
// the version field itself plus the ADR-009 expand/rotation events; the
// previous unversioned shape is v1 retroactively (absence of the field =
// v1), so old rows still render in the WebUI. Version 3 adds the ADR-010
// capability_fallback event (capability-named fallback consultation).
// Version 4 adds the model_resolved event (2026-09-21 model identity:
// routing ID → wire-name resolution). Version 5 adds the implicit_route
// event (ADR-011: no rule matched, the models-table row named the
// provider). Version 6 adds the context_override event (2026-10-06: the
// Claude Code "[1m]"/"[1M]" context-marker strip, base model routed).
type chainTrace struct {
	Version int               `json:"version"`
	Events  []json.RawMessage `json:"events"`
}

// routeTraceVersion is the current route_trace schema version (v6 adds
// the context_override event — the Claude Code "[1m]" marker strip,
// 2026-10-06; previously 5 / implicit model routes ADR-011). The WebUI
// renders route_trace as raw JSON, so bumps are additive.
const routeTraceVersion = 6

// traceField is a request's marshalled route_trace plus the in-memory
// sub-operation payloads that must never enter it (2026-10-10 Logs page).
//
// One type rather than a second parameter: every call site already passes either
// nil or a routeTrace(...) call, so the write path receives the payloads without
// a single call site changing — and no surface can forget to pass them.
type traceField struct {
	raw           json.RawMessage
	childPayloads []routing.ChildPayload
}

// routeTrace marshals the trace object from collected traces. Trace
// embeds a mutex, so events are appended from the Events() copies — the
// Trace itself is never copied.
//
// The collected sub-operation payloads ride back alongside the JSON: they are
// the child rows' bodies, and they deliberately stay out of the marshalled
// schema (see routing.Trace's payloads field).
func routeTrace(traces ...*routing.Trace) *traceField {
	t := &chainTrace{Version: routeTraceVersion, Events: make([]json.RawMessage, 0, 8)}
	tf := &traceField{}
	for _, tr := range traces {
		if tr == nil {
			continue
		}
		for _, ev := range tr.Events() {
			raw, err := json.Marshal(ev)
			if err == nil {
				t.Events = append(t.Events, raw)
			}
		}
		tf.childPayloads = append(tf.childPayloads, tr.Payloads()...)
	}
	out, err := json.Marshal(t)
	if err != nil {
		tf.raw = json.RawMessage(`{"version":5,"events":[]}`)
		return tf
	}
	tf.raw = out
	return tf
}

// lastProvider extracts the provider that actually served the request from
// the executor trace: the last attempt event names the serving (or last
// attempted) provider, correct across failovers (Analysis E).
func lastProvider(tr *routing.Trace) string {
	if tr == nil {
		return ""
	}
	var providerID string
	for _, ev := range tr.Events() {
		if ev.Type == routing.EventAttempt && ev.ProviderID != "" {
			providerID = ev.ProviderID
		}
	}
	return providerID
}

const rollingConversationWindow = int64(1800) // 30 min, see conversation.go

// writeRequestLog persists one request_logs row (plan §4.3). Called on
// success AND error paths — a request that never reached an upstream is
// still an observable request (Analysis E: fallbacks are never silent).
// ErrorRedacted carries the terminal error message with credentials never
// echoed: upstream bodies ride UpstreamError.Message, which never contains
// request headers.
//
// Logs-page redesign: the per-request meta (protocol, features,
// conversation) rides the request context (logmeta.go) — written here,
// where the outcome is known, from fields captured at decode time. The
// in-flight entry (if any) is finished here, exactly once, carrying the
// real rowid to the live stream.
func (a *api) writeRequestLog(ctx context.Context, keyID, model, providerID string, status int, latencyMS int64, usage *ir.Usage, tf *traceField, errMsg string) {
	meta := metaFromCtx(ctx)

	// The trace field carries both halves: the marshalled route_trace the row
	// stores, and the sub-operation payloads that must never enter it. A request
	// that never reached routing has neither.
	var trace json.RawMessage
	var childPayloads []routing.ChildPayload
	if tf != nil {
		trace = tf.raw
		childPayloads = tf.childPayloads
	}

	// Conversation identity (rolling window): same key + conv_key within
	// the window reuses the prior conversation_id; a store error falls
	// back to the fresh deterministic id — the log write must never break
	// the response already streamed (the same swallow-failure posture as
	// the insert below).
	var usageRaw json.RawMessage
	if usage != nil {
		if raw, err := json.Marshal(usage); err == nil {
			usageRaw = raw
		}
	}

	row := storage.RequestLog{
		Timestamp:     time.Now().Unix(),
		ClientKeyID:   keyID,
		ProviderID:    providerID,
		Model:         model,
		Status:        status,
		LatencyMS:     latencyMS,
		RouteTrace:    trace,
		ErrorRedacted: errMsg,
		Usage:         usageRaw,
	}
	if meta != nil {
		row.Features = marshalFeatures(meta.Features)
		row.Protocol = meta.Protocol
		row.ConvKey = meta.ConvKey
		if meta.ConvKey != "" {
			var prior string
			if a.store != nil {
				prior, _ = a.store.RecentConversation(ctx, keyID, meta.ConvKey,
					row.Timestamp, rollingConversationWindow)
			}
			if prior != "" {
				row.ConversationID = prior
			} else {
				row.ConversationID = convIDFromKey(meta.ConvKey)
			}
		}
	}
	if len(row.Features) == 0 {
		row.Features = json.RawMessage(`{}`)
	}
	if len(row.RouteTrace) == 0 {
		row.RouteTrace = json.RawMessage(`{}`)
	}

	// The log write must not break the response already streamed to the
	// client; a failed audit row is a server defect, not a client error.
	rowID, _ := a.store.InsertRequestLog(ctx, row)

	// Payload capture (2026-10-10 Logs page): the conversation and the answer
	// beside the metadata row, so a log entry can be READ rather than only
	// counted. Off the request path — the client already has its answer (see
	// capture.go). Same swallow-failure posture as the row above.
	a.capturePayload(meta, rowID, row.Timestamp)

	// Sub-operations (2026-10-07 Logs page): the trace's separable upstream
	// operations — failovers, capability (vision) fallback walks, emulated
	// web-tool calls — persist as their own rows under this request's id,
	// so a fallback model or a web-tool call is visible in the log instead
	// of only inside route_trace (a same-provider retry stays a timeline
	// event). Derived from the same trace just stored. Same swallow-failure
	// posture as the row above: a failed child write is a server defect,
	// not a client error.
	//
	// Each row also gets its own payload (2026-10-10): what that step sent and
	// received, so the row opens a detail view instead of a character count.
	// The insert returns the ids it allocated, which is what a payload is keyed
	// by — hence the write happening here rather than in the payload path.
	if ops := ChildOpsFromTrace(trace); len(ops) > 0 {
		// Body capture gates the caption row's excerpt as well as its payload:
		// the excerpt IS caption text, and the row's detail is persisted by the
		// insert below, before the payload write's own gate can apply.
		rows, bodies := attachChildPayloads(ops, childPayloads, a.captureEnabled(ctx))
		if ids, cerr := a.store.InsertChildLogs(ctx, rowID, rows); cerr == nil {
			for i := range bodies {
				if i < len(ids) {
					bodies[i].id = ids[i]
				}
			}
			a.captureChildPayloads(bodies, row.Timestamp)
		}
	}

	// Live stream: finish the in-flight entry (idempotent; its broadcast
	// carries the terminal row) — or, for rows without a registered entry
	// (count_tokens pre-decode, stream wired nil), publish the row
	// directly. ALL requests stay observable either way.
	if a.stream != nil {
		liveRow := reqlog.LogRow{
			ID:             rowID,
			Timestamp:      row.Timestamp,
			ClientKeyID:    row.ClientKeyID,
			ProviderID:     row.ProviderID,
			Model:          row.Model,
			Status:         row.Status,
			LatencyMS:      row.LatencyMS,
			Usage:          row.Usage,
			RouteTrace:     row.RouteTrace,
			ConversationID: row.ConversationID,
			Protocol:       row.Protocol,
			Features:       row.Features,
			ErrorRedacted:  row.ErrorRedacted,
		}
		if meta != nil && meta.finish != nil {
			a.finishInflight(meta, liveRow)
		} else {
			a.stream.PublishRow(liveRow)
		}
	}

	// usage_daily increment (Analysis C schema): the Overview dashboard's
	// data source. Same tuple as the log row, day = today (UTC). Every
	// observable request counts (+1); a failed one also counts as failure
	// (Analysis E — failure is never silent). Token deltas only when the
	// request produced usage; a request that died before an upstream saw
	// it aggregates zeros on its tuple. Same swallow-failure rule: the
	// response is already on the wire.
	inc := storage.UsageDaily{
		Day:         time.Now().UTC().Format("2006-01-02"),
		ClientKeyID: keyID,
		ProviderID:  providerID,
		Model:       model,
		Requests:    1,
		Failures:    failureDelta(status),
	}
	if usage != nil {
		inc.InputTokens = int64(usage.InputTokens)
		inc.OutputTokens = int64(usage.OutputTokens)
		inc.CacheReadTokens = int64(usage.CacheReadInputTokens)
	}
	_ = a.store.IncrUsageDaily(ctx, inc)
}

// ChildOpsFromTrace decodes a stored route_trace object into the request's
// separable sub-operations (routing.ChildOps). A trace that is absent,
// unparseable, or in a shape without the op-carrying events yields no ops —
// the same forgiving posture the WebUI's raw-JSON fallback takes, so an old
// or unexpected row can never break the log write.
func ChildOpsFromTrace(trace json.RawMessage) []storage.ChildLog {
	if len(trace) == 0 {
		return nil
	}
	var t chainTrace
	if err := json.Unmarshal(trace, &t); err != nil {
		return nil
	}
	events := make([]routing.Event, 0, len(t.Events))
	for _, raw := range t.Events {
		var ev routing.Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			continue // skip one malformed event, keep the rest
		}
		events = append(events, ev)
	}
	ops := routing.ChildOps(events)
	if len(ops) == 0 {
		return nil
	}
	out := make([]storage.ChildLog, 0, len(ops))
	for _, op := range ops {
		out = append(out, storage.ChildLog{
			TS:         time.Now().Unix(),
			Kind:       string(op.Kind),
			ProviderID: op.ProviderID,
			Model:      op.Model,
			FromRef:    op.From,
			ToRef:      op.To,
			// Detail is upstream-influenced (the emulated-tool path carries
			// the emulator's error text and a model-supplied URL) and this
			// row is a curated surface of its own, so it passes the redactor
			// exactly like ErrorRedacted — never a raw credential shape.
			Detail: childRedactor.Redact(op.Detail),
		})
	}
	return out
}

// failureDelta counts a failure for status >= 400: any client-visible
// error response (auth reject, unroutable model, upstream failure after
// failover) is a failure row on the dashboard, not only upstream errors.
func failureDelta(status int) int64 {
	if status >= 400 {
		return 1
	}
	return 0
}
