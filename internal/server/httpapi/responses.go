package httpapi

// POST /v1/responses (plan §5.1, wire facts O5–O8): the OpenAI Responses
// surface. The client body decodes through the Responses codec into IR,
// routes through the chat-wire upstream (custom provider class), and
// re-encodes into Responses wire — semantic streaming events per O7.

import (
	"net/http"
	"strings"
	"time"

	"github.com/tubeof/proxyllm/internal/auth"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/openai"
)

// handleResponses serves POST /v1/responses (streaming + non-streaming).
func (a *api) handleResponses(w http.ResponseWriter, r *http.Request) {
	ew := openAIErrorWriter()

	raw, ok := readBody(w, r)
	if !ok {
		return
	}

	req, err := openai.ResponsesCodec{}.DecodeRequest(raw)
	if err != nil {
		a.writeRouteError(w, openAIErrorWriter(), err)
		return
	}

	// Log-meta annotation + in-flight registration (Logs-page redesign):
	// one call before the stream/static split covers both runners (they
	// share r's context).
	r = a.beginRequest(r, protocolOpenAIResponses, raw, req)

	ctx := r.Context()
	keyID := auth.ClientKeyIDFromContext(ctx)

	compat := a.compatFor(ctx, req.Model)
	req, compatTrace, err := routing.ApplyCompat(compat, req)
	if err != nil {
		_, _ = a.writeRouteError(w, ew, err)
		a.writeRequestLog(ctx, keyID, req.Model, "", http.StatusBadRequest, 0,
			nil, routeTrace(compatTrace), redactedMessage(err))
		return
	}

	chain, routerTrace, err := a.router.ResolveChainFor(req.Model,
		requiredCapabilities(req), routing.WithKeyID(keyID), routing.WithRequestContext(ctx))
	if err != nil {
		_, _ = a.writeRouteError(w, ew, err)
		a.writeRequestLog(ctx, keyID, req.Model, "", ew.errorStatus(err), 0,
			nil, routeTrace(compatTrace, routerTrace), redactedMessage(err))
		return
	}

	// Model identity (2026-09-21): resolve the chain entries' routing ID to
	// the wire name; req.Model stays the client-facing ID (OP-2 echo + log).
	resolveTrace := &routing.Trace{}
	chain = a.resolveChainWireModels(ctx, chain, resolveTrace)

	if req.Stream {
		a.streamResponses(w, r, keyID, req, chain, compatTrace, routerTrace, resolveTrace)
		return
	}

	start := time.Now()
	resp, execTrace, err := a.executor.Execute(ctx, chain, req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		_, msg := a.writeRouteError(w, ew, err)
		a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace),
			ew.errorStatus(err), latency, nil,
			routeTrace(compatTrace, routerTrace, resolveTrace, execTrace), msg)
		return
	}

	// OP-2: echo the requested model name (see messages.go).
	resp.Model = req.Model
	// Capture the answer the client is about to receive (see capture.go).
	captureResponse(ctx, resp)
	out, err := openai.ResponsesCodec{}.EncodeResponse(resp)
	if err != nil {
		_, msg := a.writeRouteError(w, ew, err)
		a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace),
			http.StatusInternalServerError, latency, nil,
			routeTrace(compatTrace, routerTrace, resolveTrace, execTrace), msg)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)

	usage := resp.Usage
	a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace),
		http.StatusOK, latency, &usage,
		routeTrace(compatTrace, routerTrace, resolveTrace, execTrace), "")
}

// streamResponses relays an upstream stream as Responses semantic events
// (O7: response.created → output_text.delta / function_call_arguments
// deltas → response.completed carrying the terminal usage).
func (a *api) streamResponses(w http.ResponseWriter, r *http.Request, keyID string, req ir.Request, chain []routing.ChainEntry, traces ...*routing.Trace) {
	// Declare the event-stream response up front, before any dispatch
	// (a pre-stream failure below still substitutes a real error).
	writeSSEHeaders(w)

	ew := openAIErrorWriter()

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

	enc := openai.NewResponsesStreamEncoder(w)
	var terminalUsage *ir.Usage
	var streamErr *ir.UpstreamError
	var answer strings.Builder
	relayStream(ctx, ch, enc, &terminalUsage, &streamErr, req.Model, &answer)
	// Capture the answer the client streamed (see capture.go).
	captureResponseText(ctx, req.Model, answer.String())
	// Mid-stream outcome correction (4a): dispatch success was reported
	// at ExecuteStream; a stream that failed mid-stream still counts as
	// a breaker failure.
	a.executor.ReportStreamOutcome(lastProvider(execTrace), streamErr)

	var errMsg string
	if streamErr != nil {
		// ErrorRedacted contract: credential shapes never persist raw.
		errMsg = redactUpstreamMessage(streamErr.Message)
	}
	a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace),
		http.StatusOK, time.Since(start).Milliseconds(), terminalUsage,
		routeTrace(append(traces, execTrace)...), errMsg)
}
