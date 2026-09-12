package httpapi

// POST /v1/messages (+ ?beta=true path match, C1), POST
// /v1/messages/count_tokens (A2 heuristic), and the Anthropic-shaped half
// of GET /v1/models (C1/C3). Wire facts A2, A5–A7, A13, A14. The mock
// upstream in tests speaks OpenAI chat wire — this surface is where the
// cross-surface translation contract (OP-2) is proven.

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/proxyllm/proxyllm/internal/auth"
	"github.com/proxyllm/proxyllm/internal/routing"
	"github.com/proxyllm/proxyllm/internal/translate/anthropic"
	"github.com/proxyllm/proxyllm/internal/translate/ir"
)

// handleMessages serves POST /v1/messages (streaming + non-streaming).
// The ServeMux pattern matches the path only, so ?beta=true rides along
// naturally (C1).
func (a *api) handleMessages(w http.ResponseWriter, r *http.Request) {
	raw, ok := readBody(w, r)
	if !ok {
		return
	}

	req, err := anthropic.Codec{}.DecodeRequest(raw)
	if err != nil {
		a.writeRouteError(w, anthropicErrorWriter(), err)
		return
	}

	ctx := r.Context()
	keyID := auth.ClientKeyIDFromContext(ctx)

	compat := a.compatFor(ctx, req.Model)
	req, compatTrace, err := routing.ApplyCompat(compat, req)
	if err != nil {
		// ErrThinkingBlocked → 400 A13 envelope + log row.
		_, _ = a.writeRouteError(w, anthropicErrorWriter(), err)
		a.writeRequestLog(ctx, keyID, req.Model, "", http.StatusBadRequest, 0,
			nil, routeTrace(compatTrace), redactedMessage(err))
		return
	}

	chain, routerTrace, err := a.router.ResolveChainFor(req.Model,
		requiredCapabilities(req), routing.WithKeyID(keyID))
	if err != nil {
		_, _ = a.writeRouteError(w, anthropicErrorWriter(), err)
		a.writeRequestLog(ctx, keyID, req.Model, "", mapRouteErrorStatus(err), 0,
			nil, routeTrace(compatTrace, routerTrace), redactedMessage(err))
		return
	}

	if req.Stream {
		a.streamMessages(w, r, keyID, req, chain, compatTrace, routerTrace)
		return
	}

	start := time.Now()
	resp, execTrace, err := a.executor.Execute(ctx, chain, req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		_, msg := a.writeRouteError(w, anthropicErrorWriter(), err)
		a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace),
			mapRouteErrorStatus(err), latency, nil,
			routeTrace(compatTrace, routerTrace, execTrace), msg)
		return
	}

	// OP-2: the client-facing response echoes the requested model name —
	// never the upstream-internal one (alias/routed model names stay
	// invisible to the client).
	resp.Model = req.Model
	out, err := anthropic.Codec{}.EncodeResponse(resp)
	if err != nil {
		_, msg := a.writeRouteError(w, anthropicErrorWriter(), err)
		a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace),
			http.StatusInternalServerError, latency, nil,
			routeTrace(compatTrace, routerTrace, execTrace), msg)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)

	usage := resp.Usage
	a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace),
		http.StatusOK, latency, &usage,
		routeTrace(compatTrace, routerTrace, execTrace), "")
}

// streamMessages relays an upstream stream as named Anthropic events
// (A5/A6), terminating with message_stop. Mid-stream upstream failures
// keep HTTP 200 (the SSE stream already started) and surface as
// `event: error` with a normalized error type (A13).
func (a *api) streamMessages(w http.ResponseWriter, r *http.Request, keyID string, req ir.Request, chain []routing.ChainEntry, traces ...*routing.Trace) {
	ctx := r.Context()

	start := time.Now()
	ch, cancel, execTrace, err := a.executor.ExecuteStream(ctx, chain, req)
	if err != nil {
		_, msg := a.writeRouteError(w, anthropicErrorWriter(), err)
		a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace),
			mapRouteErrorStatus(err), time.Since(start).Milliseconds(), nil,
			routeTrace(append(traces, execTrace)...), msg)
		return
	}
	defer cancel()

	enc := anthropic.NewStreamEncoder(w)
	var terminalUsage *ir.Usage
	var streamErr *ir.UpstreamError
	relayStream(ctx, ch, enc, &terminalUsage, &streamErr, req.Model)

	var errMsg string
	if streamErr != nil {
		errMsg = streamErr.Message
	}
	a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace),
		http.StatusOK, time.Since(start).Milliseconds(), terminalUsage,
		routeTrace(append(traces, execTrace)...), errMsg)
}

// handleCountTokens serves POST /v1/messages/count_tokens (A2/C1). With an
// OpenAI-shaped upstream, native Anthropic counting is impossible, so the
// heuristic estimate (chars/4 over the decoded text) answers — marked
// `estimation: "heuristic"`, never presented as an exact count. The
// heuristic runs locally: no upstream dispatch, no request-log row (no
// upstream attempt happened).
func (a *api) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	raw, ok := readBody(w, r)
	if !ok {
		return
	}

	req, err := anthropic.Codec{}.DecodeRequest(raw)
	if err != nil {
		a.writeRouteError(w, anthropicErrorWriter(), err)
		return
	}

	estimate := countTokensHeuristic(req)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"input_tokens":` + itoa(estimate) +
		`,"estimation":"heuristic"}`))
}

// countTokensHeuristic estimates input tokens as chars/4 over system +
// messages + tool schemas (Analysis D, calibrated against the observed
// ~4 chars/token ratio; C1 mandates the heuristic marking).
func countTokensHeuristic(req ir.Request) int {
	chars := 0
	for _, sp := range req.System {
		chars += len(sp.Text)
	}
	for _, msg := range req.Messages {
		for _, part := range msg.Parts {
			switch p := part.(type) {
			case ir.TextPart:
				chars += len(p.Text)
			case ir.ToolCallPart:
				chars += len(p.Input)
			case ir.ToolResultPart:
				for _, sub := range p.Content {
					if t, ok := sub.(ir.TextPart); ok {
						chars += len(t.Text)
					}
				}
			case ir.ImagePart:
				// Images are not char-countable; a flat image allowance.
				chars += 1024
			}
		}
	}
	for _, tool := range req.Tools {
		chars += len(tool.Name) + len(tool.Description) + len(tool.InputSchema)
	}
	tokens := chars / 4
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

// itoa is the tiny integer formatter keeping this file free of strconv
// imports beyond this helper.
func itoa(n int) string {
	return strings.TrimSpace(jsonInt(n))
}

// jsonInt renders n as a JSON integer literal.
func jsonInt(n int) string {
	b, err := json.Marshal(n)
	if err != nil {
		return "0"
	}
	return string(b)
}
