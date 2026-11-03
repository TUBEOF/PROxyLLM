package httpapi

// POST /v1/responses: the OpenAI Responses
// surface. The client body decodes through the Responses codec into IR,
// routes through the chat-wire upstream (custom provider class), and
// re-encodes into Responses wire — semantic streaming events.

import (
	"net/http"
	"time"

	"github.com/tubeof/proxyllm/internal/auth"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/openai"
)

// httpHistoryLimit bounds the process-wide previous_response_id store. One
// entry is a full conversation, so the cap is deliberately small: a client
// that chains more turns than this gets the documented 404, never unbounded
// memory growth.
const httpHistoryLimit = 64

// historyKey scopes one client's response ids: an id minted for one client
// key must never resolve for another.
func historyKey(keyID, responseID string) string { return keyID + "\x00" + responseID }

// handleResponses serves POST /v1/responses (streaming + non-streaming).
func (a *api) handleResponses(w http.ResponseWriter, r *http.Request) {
	ew := openAIErrorWriter()

	raw, ok := readBody(w, r)
	if !ok {
		return
	}

	req, err := openai.ResponsesCodec{}.DecodeRequest(raw)
	if err != nil {
		status, msg := a.writeDecodeError(w, ew, err)
		a.writeRequestLog(r.Context(), auth.ClientKeyIDFromContext(r.Context()),
			"", "", status, 0, nil, nil, msg)
		return
	}

	// Log-meta annotation + in-flight registration (Logs-page redesign):
	// one call before the stream/static split covers both runners (they
	// share r's context).
	r = a.beginRequest(r, protocolOpenAIResponses, raw, req)

	ctx := r.Context()
	keyID := auth.ClientKeyIDFromContext(ctx)

	// A previous_response_id chains this turn onto an earlier one. The proxy
	// asks no upstream to retain a response, so the earlier turn exists only
	// in this process's bounded history; an id it does not hold is the
	// documented "not found", never a silent answer from an orphaned turn
	// (which is what a client sees when its tool result arrives without the
	// tool call that produced it).
	if req.PreviousResponseID != "" {
		prior, ok := a.history.get(historyKey(keyID, req.PreviousResponseID))
		if !ok {
			ew.write(w, http.StatusNotFound, "invalid_request_error",
				"previous response with this previous_response_id is not available; send the full input")
			a.writeRequestLog(ctx, keyID, req.Model, "", http.StatusNotFound, 0,
				nil, nil, "previous_response_id not found")
			return
		}
		req.Messages = append(prior, req.Messages...)
		// The id is the proxy's own mint: forwarding it to an upstream that
		// never issued it earns a 400 (the WebSocket path strips it for the
		// same reason).
		req.PreviousResponseID = ""
	}

	if err := routing.RequireMessages(req); err != nil {
		_, _ = a.writeRouteError(w, ew, err)
		a.writeRequestLog(ctx, keyID, req.Model, "", http.StatusBadRequest, 0,
			nil, nil, redactedMessage(err))
		return
	}

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

	// Model identity: resolve the chain entries' routing ID to the wire
	// name; req.Model stays the client-facing ID (echoed to the client + logged).
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

	// Echo the requested model name (see messages.go).
	resp.Model = req.Model
	// Remember the turn so the client can chain it with previous_response_id.
	a.history.put(historyKey(keyID, resp.ID),
		append(append([]ir.Message{}, req.Messages...),
			ir.Message{Role: ir.RoleAssistant, Parts: resp.Content}))
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
// (response.created → output_text.delta / function_call_arguments
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
	parts, answer, terminalUsage, streamErr := relayWSStream(ctx, ch, enc, req.Model)
	// Remember the turn so the client can chain it with previous_response_id.
	a.history.put(historyKey(keyID, enc.ResponseID()),
		append(append([]ir.Message{}, req.Messages...),
			ir.Message{Role: ir.RoleAssistant, Parts: parts}))
	// Capture the answer the client streamed (see capture.go).
	captureResponseText(ctx, req.Model, answer)
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
