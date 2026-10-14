package httpapi

// POST /v1/messages (+ ?beta=true path match, C1), POST
// /v1/messages/count_tokens (A2 heuristic), and the Anthropic-shaped half
// of GET /v1/models (C1/C3). Wire facts A2, A5–A7, A13, A14. The mock
// upstream in tests speaks OpenAI chat wire — this surface is where the
// cross-surface translation contract (OP-2) is proven.

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tubeof/proxyllm/internal/auth"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/anthropic"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// handleMessages serves POST /v1/messages (streaming + non-streaming).
// The ServeMux pattern matches the path only, so ?beta=true rides along
// naturally (C1).
func (a *api) handleMessages(w http.ResponseWriter, r *http.Request) {
	ew := anthropicErrorWriter()

	raw, ok := readBody(w, r)
	if !ok {
		return
	}

	req, err := anthropic.Codec{}.DecodeRequest(raw)
	if err != nil {
		a.writeRouteError(w, anthropicErrorWriter(), err)
		return
	}

	// Log-meta annotation + in-flight registration (Logs-page redesign).
	r = a.beginRequest(r, protocolAnthropic, raw, req)

	ctx := r.Context()
	keyID := auth.ClientKeyIDFromContext(ctx)

	// Global non-Claude switch (§6.4, ADR-008): gate before compat/routing
	// so block dispatches nothing. The gate's events ride the same trace
	// the compat application builds — one route_trace, never silent.
	compatTrace := &routing.Trace{}
	if err := a.applyNonClaudeGate(ctx, req.Model, compatTrace); err != nil {
		_, _ = a.writeRouteError(w, ew, err)
		a.writeRequestLog(ctx, keyID, req.Model, "", http.StatusBadRequest, 0,
			nil, routeTrace(compatTrace), redactedMessage(err))
		return
	}

	compat := a.compatFor(ctx, req.Model)
	req, applyTrace, err := routing.ApplyCompat(compat, req)
	if err != nil {
		// ErrThinkingBlocked → 400 A13 envelope + log row.
		_, _ = a.writeRouteError(w, ew, err)
		a.writeRequestLog(ctx, keyID, req.Model, "", http.StatusBadRequest, 0,
			nil, routeTrace(compatTrace, applyTrace), redactedMessage(err))
		return
	}

	chain, routerTrace, err := a.router.ResolveChainFor(req.Model,
		requiredCapabilities(req), routing.WithKeyID(keyID), routing.WithRequestContext(ctx))
	if err != nil {
		_, _ = a.writeRouteError(w, ew, err)
		a.writeRequestLog(ctx, keyID, req.Model, "", ew.errorStatus(err), 0,
			nil, routeTrace(compatTrace, applyTrace, routerTrace), redactedMessage(err))
		return
	}

	// Model identity (2026-09-21): the chain entries' routing ID resolves
	// to the upstream wire name for dispatch; req.Model (the client-facing
	// ID) is unchanged for the OP-2 echo and the log row.
	resolveTrace := &routing.Trace{}
	chain = a.resolveChainWireModels(ctx, chain, resolveTrace)

	if req.Stream {
		a.streamMessages(w, r, keyID, req, chain, compatTrace, applyTrace, routerTrace, resolveTrace)
		return
	}

	start := time.Now()
	resp, execTrace, err := a.executor.Execute(ctx, chain, req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		_, msg := a.writeRouteError(w, ew, err)
		a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace),
			ew.errorStatus(err), latency, nil,
			routeTrace(compatTrace, applyTrace, routerTrace, resolveTrace, execTrace), msg)
		return
	}

	// OP-2: the client-facing response echoes the requested model name —
	// never the upstream-internal one (alias/routed model names stay
	// invisible to the client).
	resp.Model = req.Model
	// Capture the answer the client is about to receive, under the same model
	// name it sees.
	captureResponse(ctx, resp)
	out, err := anthropic.Codec{}.EncodeResponse(resp)
	if err != nil {
		_, msg := a.writeRouteError(w, ew, err)
		a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace),
			http.StatusInternalServerError, latency, nil,
			routeTrace(compatTrace, applyTrace, routerTrace, resolveTrace, execTrace), msg)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)

	usage := resp.Usage
	a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace),
		http.StatusOK, latency, &usage,
		routeTrace(compatTrace, applyTrace, routerTrace, resolveTrace, execTrace), "")
}

// streamMessages relays an upstream stream as named Anthropic events
// (A5/A6), terminating with message_stop. Mid-stream upstream failures
// keep HTTP 200 (the SSE stream already started) and surface as
// `event: error` with a normalized error type (A13).
func (a *api) streamMessages(w http.ResponseWriter, r *http.Request, keyID string, req ir.Request, chain []routing.ChainEntry, traces ...*routing.Trace) {
	// Declare the event-stream response up front, before any dispatch
	// (a pre-stream failure below still substitutes a real error).
	writeSSEHeaders(w)

	ew := anthropicErrorWriter()

	ctx := r.Context()

	start := time.Now()
	ch, cancel, execTrace, err := a.executor.ExecuteStream(ctx, chain, req)
	if err != nil {
		_, msg := a.writeRouteError(w, ew, err)
		a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace),
			ew.errorStatus(err), time.Since(start).Milliseconds(), nil,
			routeTrace(append(traces, execTrace)...), msg)
		return
	}
	defer cancel()

	enc := anthropic.NewStreamEncoder(w)
	var terminalUsage *ir.Usage
	var streamErr *ir.UpstreamError
	var answer strings.Builder
	relayStream(ctx, ch, enc, &terminalUsage, &streamErr, req.Model, &answer)
	// Capture the answer the client streamed (see capture.go).
	captureResponseText(ctx, req.Model, answer.String())
	// Mid-stream outcome correction (4a): dispatch success was reported
	// at ExecuteStream; a stream that failed mid-stream still counts as
	// a breaker failure.
	var errMsg string
	if streamErr != nil {
		// ErrorRedacted contract: credential shapes never persist raw.
		errMsg = redactUpstreamMessage(streamErr.Message)
	}
	// An abandoned request is still logged — it really happened — but it is
	// neither a success nor a failure for the provider, because nothing was
	// learned about it. Reporting the dispatch success again here would clear a
	// breaker the client's own departure had nothing to do with.
	status := http.StatusOK
	if gone, abandoned := abandonmentStatus(ctx); abandoned {
		status = gone
	} else {
		// Mid-stream outcome correction (4a): dispatch success was reported at
		// ExecuteStream; a stream that failed mid-stream still counts as a
		// breaker failure.
		a.executor.ReportStreamOutcome(lastProvider(execTrace), streamErr)
	}
	a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace),
		status, time.Since(start).Milliseconds(), terminalUsage,
		routeTrace(append(traces, execTrace)...), errMsg)
}

// handleCountTokens serves POST /v1/messages/count_tokens (A2/C1). With an
// OpenAI-shaped upstream, native Anthropic counting is impossible, so the
// heuristic estimate (chars/4 over the decoded text) answers — marked
// `estimation: "heuristic"`, never presented as an exact count. The
// heuristic runs locally with no upstream dispatch, but the request is
// still observable: a log row with the estimate records it (Analysis E).
func (a *api) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	ew := anthropicErrorWriter()

	raw, ok := readBody(w, r)
	if !ok {
		return
	}

	req, err := anthropic.Codec{}.DecodeRequest(raw)
	if err != nil {
		_, msg := a.writeRouteError(w, ew, err)
		a.writeRequestLog(r.Context(), auth.ClientKeyIDFromContext(r.Context()),
			"", "", http.StatusBadRequest, 0, nil, nil, msg)
		return
	}

	// Observable request (Analysis E): the heuristic runs locally, but the
	// row still records it — annotated without in-flight registration
	// (no upstream dispatch to trace).
	r = a.beginRequest(r, protocolAnthropic, raw, req)

	estimate := req.EstimateTokens()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"input_tokens":` + strconv.Itoa(estimate) +
		`,"estimation":"heuristic"}`))

	a.writeRequestLog(r.Context(), auth.ClientKeyIDFromContext(r.Context()),
		req.Model, "", http.StatusOK, 0, nil, nil, "")
}

// The estimator itself lives in the IR package (ir.Request.EstimateTokens,
// moved there 2026-10-09): the routing executor's capability-fallback
// context gate compares the same figure against a fallback model's declared
// window, and two copies of a heuristic drift apart silently.
