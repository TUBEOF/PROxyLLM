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
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// Params wires the API surfaces to their collaborators.
type Params struct {
	// Store persists models (compat policies) and request logs.
	Store *storage.Store

	// Router resolves fallback chains from the routing rules.
	Router *routing.Router

	// Executor dispatches requests through the chain with failover.
	Executor *routing.Executor
}

// api holds the wired collaborators.
type api struct {
	store    *storage.Store
	router   *routing.Router
	executor *routing.Executor
}

// New assembles the client-facing HTTP handler (plan §5.1).
func New(p Params) http.Handler {
	a := &api{store: p.Store, router: p.Router, executor: p.Executor}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", a.handleChat)
	mux.HandleFunc("POST /v1/responses", a.handleResponses)
	mux.HandleFunc("POST /v1/messages", a.handleMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", a.handleCountTokens)
	mux.HandleFunc("GET /v1/models", a.handleModels)
	return mux
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

// compatFor loads the per-model compat policy. A model without a row (or
// with an unparsable compat blob) is not an error: it routes with the
// default (strip) policy — the store is eventually consistent with the
// WebUI editor, and requests must not hard-fail on a missing row.
func (a *api) compatFor(ctx context.Context, model string) routing.Compat {
	var c routing.Compat
	m, err := a.store.GetModel(ctx, model)
	if err != nil {
		return c // zero value: strip default, no clamp
	}
	var w wireCompat
	if len(m.Compat) > 0 {
		_ = json.Unmarshal(m.Compat, &w)
	}
	c.ThinkingHandling = w.ThinkingHandling
	c.ServerToolEmulation = w.ServerToolEmulation
	c.MaxTokensClamp = w.MaxTokensClamp
	c.ContextWindow = w.ContextWindow
	c.CacheControl = w.CacheControl
	return c
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
// {"events":[...]} with the compat, router, and executor events
// concatenated in order (plan §4.3 observability — an object, so the row
// can grow metadata without a breaking change).
type chainTrace struct {
	Events []json.RawMessage `json:"events"`
}

// routeTrace marshals the trace object from collected traces. Trace
// embeds a mutex, so events are appended from the Events() copies — the
// Trace itself is never copied.
func routeTrace(traces ...*routing.Trace) json.RawMessage {
	t := &chainTrace{Events: make([]json.RawMessage, 0, 8)}
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
	}
	out, err := json.Marshal(t)
	if err != nil {
		return json.RawMessage(`{"events":[]}`)
	}
	return out
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

// writeRequestLog persists one request_logs row (plan §4.3). Called on
// success AND error paths — a request that never reached an upstream is
// still an observable request (Analysis E: fallbacks are never silent).
// ErrorRedacted carries the terminal error message with credentials never
// echoed: upstream bodies ride UpstreamError.Message, which never contains
// request headers.
func (a *api) writeRequestLog(ctx context.Context, keyID, model, providerID string, status int, latencyMS int64, usage *ir.Usage, trace json.RawMessage, errMsg string) {
	row := storage.RequestLog{
		Timestamp:     time.Now().Unix(),
		ClientKeyID:   keyID,
		ProviderID:    providerID,
		Model:         model,
		Status:        status,
		LatencyMS:     latencyMS,
		RouteTrace:    trace,
		ErrorRedacted: errMsg,
	}
	if usage != nil {
		if raw, err := json.Marshal(usage); err == nil {
			row.Usage = raw
		}
	}
	// The log write must not break the response already streamed to the
	// client; a failed audit row is a server defect, not a client error.
	_, _ = a.store.InsertRequestLog(ctx, row)

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

// failureDelta counts a failure for status >= 400: any client-visible
// error response (auth reject, unroutable model, upstream failure after
// failover) is a failure row on the dashboard, not only upstream errors.
func failureDelta(status int) int64 {
	if status >= 400 {
		return 1
	}
	return 0
}
