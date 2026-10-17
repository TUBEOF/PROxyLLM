// Package httpapi implements the client-facing API surfaces:
// the OpenAI-compatible surface (/v1/chat/completions, /v1/responses,
// GET /v1/models) and the Anthropic-compatible surface (/v1/messages,
// /v1/messages/count_tokens, dual-shape model discovery). Requests decode
// into the canonical IR, route through the Router + Executor, and
// re-encode into the requesting protocol — same-surface relay and
// cross-surface translation share one pipeline (ADR-004 hub-and-spoke).
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

	// ClientWebTools is the client-tool → emulated-family mapping the
	// Anthropic surface claims (settings KV `client_web_tools`; missing key =
	// DefaultClientWebTools). The server refreshes it on settings reloads via
	// SetClientWebTools.
	ClientWebTools map[string]string

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

	// clientWebTools is the live client-tool → family mapping (immutable
	// snapshot, swapped by SetClientWebTools on settings writes).
	clientWebTools atomic.Value // map[string]string
}

// API is the assembled client-facing handler (New's return): the mux itself,
// with the refresh points the server's admin reload hooks need.
type API struct {
	http.Handler
	a *api
}

// SetClientWebTools swaps the client-tool mapping (the settings reload calls
// it). Atomic — the next request picks up the operator's write.
func (p *API) SetClientWebTools(m map[string]string) {
	p.a.clientWebTools.Store(normalizeClientWebTools(m))
}

// DefaultClientWebTools is the shipped mapping: Claude Code sends its web
// tools as ordinary CLIENT tools (WebFetch, WebSearch) and fetches in the CLI
// itself, so a proxy that only understood the dated server-tool definitions
// would never see a web-tool request from it. The names are the client's wire
// fact — the `client_web_tools` settings KV overrides them (an empty object
// turns the claim off).
func DefaultClientWebTools() map[string]string {
	return map[string]string{"WebFetch": "web_fetch", "WebSearch": "web_search"}
}

// normalizeClientWebTools copies the mapping and drops entries that name
// nothing; nil reads as "claim nothing".
func normalizeClientWebTools(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for name, family := range m {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(family) == "" {
			continue
		}
		out[name] = family
	}
	return out
}

// New assembles the client-facing HTTP handler.
func New(p Params) *API {
	a := &api{store: p.Store, router: p.Router, executor: p.Executor, stream: p.Stream}
	a.clientWebTools.Store(normalizeClientWebTools(p.ClientWebTools))

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", a.handleChat)
	mux.HandleFunc("POST /v1/responses", a.handleResponses)
	mux.HandleFunc("POST /v1/messages", a.handleMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", a.handleCountTokens)
	// Decision surfaces (decision models): the systemone (Jev) and the
	// OpenAI Decisions shapes, both translated through the IR.
	mux.HandleFunc("POST /v1/systemone", a.handleSystemOne)
	mux.HandleFunc("POST /v1/decisions", a.handleDecisions)
	mux.HandleFunc("GET /v1/models", a.handleModels)
	return &API{Handler: mux, a: a}
}

// --- shared pipeline ---

// wireCompat is the JSON-tagged mirror of routing.Compat for decoding
// models.compat rows (routing.Compat deliberately has no JSON tags — the
// storage wire shape belongs to the HTTP layer).
type wireCompat struct {
	ThinkingHandling    string `json:"thinking_handling"`
	ServerToolEmulation string `json:"server_tool_emulation"`
	MaxTokensClamp      int    `json:"max_tokens_clamp"`
	ContextWindow       int    `json:"context_window"`
	CacheControl        string `json:"cache_control"`
}

// liveClientWebTools returns the live client-tool → family mapping.
func (a *api) liveClientWebTools() map[string]string {
	if v, ok := a.clientWebTools.Load().(map[string]string); ok {
		return v
	}
	return nil
}

// compatFor loads the compat policy of the model the request will LAND on.
// The model row's capabilities drive the policy (the WebUI compat editor is
// REMOVED — the defaults are internal policy, capability-aware): a stored
// compat blob still wins when present (legacy rows keep their
// explicitly-set behavior — read-only, the SPA no longer writes this field).
//
// The landed model is what the policy belongs to, so the lookup follows the
// same two shifts the router applies before it routes: the Claude Code
// context marker is stripped, then the alias table is followed ONE hop. A
// client-typed ID such as claude-haiku-5-5 has no row of its own — its policy
// lives on the alias target's row, and reading the typed name instead would
// silently drop to the defaults (wrong emulation policy, wrong thinking
// handling) while the request routes as the target. Missing row or a failed
// read is not an error: the request routes with the DERIVED defaults — the
// store is eventually consistent, and requests must not hard-fail on a
// missing row.
func (a *api) compatFor(ctx context.Context, model string) routing.Compat {
	// Follow routing's context-marker strip: a suffixed request
	// ("glm-5.3-flash[1M]") carries no row of its own — the operator's
	// compat policy lives on the base row and must apply (otherwise the
	// marker would silently bypass thinking handling, clamps, emulation).
	base, _ := routing.StripContextMarker(model)
	// ... and routing's alias hop: the policy is keyed by the landed model.
	if a.router != nil {
		if target, ok := a.router.AliasTarget(base); ok {
			base = target
		}
	}
	var c routing.Compat
	m, err := a.store.GetModel(ctx, base)
	if err != nil {
		// No row for the landed model: the derived defaults below still
		// apply. An unknown model is not an unconfigured one.
		m = storage.Model{}
	}
	var w wireCompat
	if len(m.Compat) > 0 {
		_ = json.Unmarshal(m.Compat, &w)
	}
	c.ThinkingHandling = defaultThinkingFor(m.Capabilities, w.ThinkingHandling)
	c.ServerToolEmulation = emulationFor(w.ServerToolEmulation)
	c.MaxTokensClamp = w.MaxTokensClamp
	c.ContextWindow = w.ContextWindow
	c.CacheControl = defaultCacheControl(w.CacheControl)
	return c
}

// defaultThinkingFor derives the thinking policy from the model row's
// capabilities when the stored blob left it unset (or was never written
// again): a thinking-capable target maps the budget to the reasoning
// effort it actually understands; everything else strips (signatures
// must never reach a non-thinking target).
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
//
// Two-step on purpose: the blob carries a `pinned` OBJECT beside the boolean
// flags (ADR-014's operator-override map), so decoding straight into
// map[string]bool failed on every row that had ever LEARNED a capability —
// which silently read as "no capabilities" and stripped thinking handling from
// exactly the rows that had been in use. Unknown shapes are skipped, not
// fatal: only a real boolean counts as a flag.
func capabilityFlags(capsJSON json.RawMessage) []string {
	if len(capsJSON) == 0 {
		return nil
	}
	var blob map[string]json.RawMessage
	if err := json.Unmarshal(capsJSON, &blob); err != nil {
		return nil
	}
	present := make([]string, 0, len(blob))
	for name, raw := range blob {
		var on bool
		if err := json.Unmarshal(raw, &on); err == nil && on {
			present = append(present, name)
		}
	}
	return present
}

// defaultCacheControl pins the enforced cache-marker default: forward
// markers only Anthropic→Anthropic (the caller decides by the target's
// surface — for all other targets the stored value stays "" = the drop
// default through compat.go).
func defaultCacheControl(stored string) string {
	if stored == routing.CacheControlForward {
		return routing.CacheControlForward
	}
	return "" // compat.go: "" = drop for non-Anthropic targets
}

// emulationFor pins the emulation default: emulate-firecrawl for EVERY target
// unless the row explicitly pinned `off`. The web-tool families are served by
// the PROXY's web-tool accounts, never by a chat model's upstream — a dated
// server-tool definition cannot ride to a provider that has never heard of it,
// and letting it ride has only bad outcomes: the router's capability filter
// skips every account that does not declare native web_search/web_fetch (the
// request dies 404 before any fallback runs), or the model calls a function
// the client cannot answer. A stored legacy `native` therefore reads as
// emulate — the editor that could write it is gone, and no provider type in
// this proxy speaks the server-tool wire. With no web-tool account configured
// every emulated call degrades to the documented `unavailable` error result,
// so the request still routes and the model can tell the client it could not
// look the request up.
func emulationFor(stored string) string {
	if stored == "" || stored == routing.EmulationNative {
		return routing.EmulationEmulateFirecrawl
	}
	return stored
}

// Settings keys read by this package (the SPA Settings surface writes them
// into the same settings KV the server's DB override layer owns).
const (
	settingKeyNonClaude = "anthropic_surface_non_claude"
)

// Non-Claude gate values (the global switch, ADR-008).
const (
	gateAllow = "allow"
	gateWarn  = "warn"
	gateBlock = "block"
)

// ErrNonClaudeBlocked is the terminal error for the block gate.
var ErrNonClaudeBlocked = errors.New("httpapi: non-Claude models are blocked on the Anthropic surface")

// nonClaudeGate reads the global switch from the settings KV. Any missing,
// unreadable, or invalid value is allow — the safety net must never
// de-route on store noise (default allow).
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
// convention across the published catalog; anything else on the
// Anthropic surface is a non-Claude target by definition.
func isClaudeModel(model string) bool {
	return strings.HasPrefix(model, "claude-")
}

// applyNonClaudeGate enforces the global switch: a non-Claude
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
// (the capability filter; model-family identity).
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
// events concatenated in order (observability — an object, so
// the row can grow metadata without a breaking change). Version 2 adds
// the version field itself plus the ADR-009 expand/rotation events; the
// previous unversioned shape is v1 retroactively (absence of the field =
// v1), so old rows still render in the WebUI. Version 3 adds the ADR-010
// capability_fallback event (capability-named fallback consultation).
// Version 4 adds the model_resolved event (model identity:
// routing ID → wire-name resolution). Version 5 adds the implicit_route
// event (ADR-011: no rule matched, the models-table row named the
// provider). Version 6 adds the context_override event (the
// Claude Code "[1m]"/"[1M]" context-marker strip, base model routed).
// Version 7 adds the emulated_tool_attempt event (one account's
// outcome inside an emulated web-tool invocation — the account walk is inside
// the pool, so the log needs it to explain a web tool that found nothing).
// Version 8 adds the per-operation facts the sub-operation rows report: the
// describer call's latency and token usage, the emulating account and duration
// of a web-tool call, and the cache-hit marker that distinguishes a substituted
// description which cost nothing from one that was just produced.
type chainTrace struct {
	Version int               `json:"version"`
	Events  []json.RawMessage `json:"events"`
}

// routeTraceVersion is the current route_trace schema version (v8 adds the
// per-operation facts — usage, latency and the cache-hit marker — that the
// sub-operation rows of the log render; previously 7 / emulated_tool_attempt,
// 6 / context_override, 5 / implicit model routes ADR-011). The WebUI renders
// route_trace as raw JSON, so bumps are additive.
const routeTraceVersion = 8

// traceField is a request's marshalled route_trace plus the in-memory
// sub-operation payloads that must never enter it (Logs page).
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
// attempted) provider, correct across failovers.
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

// writeRequestLog persists one request_logs row. Called on
// success AND error paths — a request that never reached an upstream is
// still an observable request (fallbacks are never silent).
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
	// The record outlives the request by design. A client that hung up still
	// made a request, and this row is the only trace of it — so the bookkeeping
	// below (the row, its sub-operations, the usage counter) is detached from the
	// request's cancellation instead of being abandoned with it. The values on
	// the context are still the caller's: the log meta arrives through it.
	ctx = context.WithoutCancel(ctx)

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

	// Payload capture (Logs page): the conversation and the answer
	// beside the metadata row, so a log entry can be READ rather than only
	// counted. Off the request path — the client already has its answer (see
	// capture.go). Same swallow-failure posture as the row above.
	a.capturePayload(meta, rowID, row.Timestamp)

	// Sub-operations (Logs page): the trace's separable upstream
	// operations — failovers, capability (vision) fallback walks, emulated
	// web-tool calls — persist as their own rows under this request's id,
	// so a fallback model or a web-tool call is visible in the log instead
	// of only inside route_trace (a same-provider retry stays a timeline
	// event). Derived from the same trace just stored. Same swallow-failure
	// posture as the row above: a failed child write is a server defect,
	// not a client error.
	//
	// Each row also gets its own payload: what that step sent and
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

	// usage_daily increment: the Overview dashboard's
	// data source. Same tuple as the log row, day = today (UTC). Every
	// observable request counts (+1); a failed one also counts as failure
	// (failure is never silent). Token deltas only when the
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
		usage := json.RawMessage(`{}`)
		if op.Usage != nil {
			if raw, err := json.Marshal(op.Usage); err == nil {
				usage = raw
			}
		}
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
			Detail:    childRedactor.Redact(op.Detail),
			LatencyMS: op.LatencyMS,
			Usage:     usage,
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
