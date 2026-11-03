package httpapi

// Native Gemini client surface: POST /v1beta/models/{model}:generateContent
// (JSON) and :streamGenerateContent?alt=sse (SSE). The same IR pipeline
// serves it; only the wire codec, the path convention (the model rides the
// URL, never the body) and the error shape differ from the other surfaces.

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/tubeof/proxyllm/internal/auth"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/gemini"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// handleGemini serves the Gemini generateContent paths registered under
// /v1beta/ (the segment-carried ":action" suffix cannot ride a ServeMux
// pattern).
func (a *api) handleGemini(w http.ResponseWriter, r *http.Request) {
	model, action, ok := parseGeminiPath(r.URL.Path)
	if !ok {
		writeGeminiError(w, http.StatusNotFound, errTypeNotFound, "unknown Gemini endpoint: "+r.URL.Path)
		return
	}
	switch action {
	case "generateContent":
		a.runGeminiContent(w, r, model, false)
	case "streamGenerateContent":
		a.runGeminiContent(w, r, model, true)
	default:
		writeGeminiError(w, http.StatusNotFound, errTypeNotFound, "unknown Gemini action "+action)
	}
}

// parseGeminiPath splits "/v1beta/models/{model}:{action}" into its parts.
func parseGeminiPath(path string) (model, action string, ok bool) {
	const prefix = "/v1beta/models/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	model, action, found := strings.Cut(strings.TrimPrefix(path, prefix), ":")
	if !found || model == "" || action == "" {
		return "", "", false
	}
	return model, action, true
}

// runGeminiContent runs the shared pipeline for one generateContent/stream
// round trip.
func (a *api) runGeminiContent(w http.ResponseWriter, r *http.Request, model string, stream bool) {
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	req, err := gemini.Codec{}.DecodeRequest(raw)
	if err != nil {
		status, msg := a.writeDecodeError(w, geminiErrorWriter(), err)
		a.writeRequestLog(r.Context(), auth.ClientKeyIDFromContext(r.Context()),
			"", "", status, 0, nil, nil, msg)
		return
	}
	req.Model = model // the model rides the URL path, never the body
	if stream {
		req.Stream = true
	}

	r = a.beginRequest(r, protocolGemini, raw, req)
	ctx := r.Context()
	keyID := auth.ClientKeyIDFromContext(ctx)
	ew := geminiErrorWriter()

	if err := routing.RequireMessages(req); err != nil {
		_, msg := a.writeRouteError(w, ew, err)
		a.writeRequestLog(ctx, keyID, req.Model, "", http.StatusBadRequest, 0,
			nil, nil, msg)
		return
	}

	compat := a.compatFor(ctx, req.Model)
	req, compatTrace, err := routing.ApplyCompat(compat, req)
	if err != nil {
		_, msg := a.writeRouteError(w, ew, err)
		a.writeRequestLog(ctx, keyID, req.Model, "", http.StatusBadRequest, 0,
			nil, routeTrace(compatTrace), msg)
		return
	}

	chain, routerTrace, err := a.router.ResolveChainFor(req.Model,
		requiredCapabilities(req), routing.WithKeyID(keyID), routing.WithRequestContext(ctx))
	if err != nil {
		_, msg := a.writeRouteError(w, ew, err)
		a.writeRequestLog(ctx, keyID, req.Model, "", ew.errorStatus(err), 0,
			nil, routeTrace(compatTrace, routerTrace), msg)
		return
	}
	resolveTrace := &routing.Trace{}
	chain = a.resolveChainWireModels(ctx, chain, resolveTrace)

	if req.Stream {
		a.streamGemini(w, r, keyID, req, chain, compatTrace, routerTrace, resolveTrace)
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
	resp.Model = req.Model
	captureResponse(ctx, resp)
	out, err := gemini.Codec{}.EncodeResponse(resp)
	if err != nil {
		_, msg := a.writeRouteError(w, ew, err)
		a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace),
			http.StatusInternalServerError, latency, nil,
			routeTrace(compatTrace, routerTrace, resolveTrace, execTrace), msg)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	//nolint:gosec // G705: the codec JSON-marshals the IR response (html-escaping on), never raw client input.
	_, _ = w.Write(out)
	usage := resp.Usage
	a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace),
		http.StatusOK, latency, &usage,
		routeTrace(compatTrace, routerTrace, resolveTrace, execTrace), "")
}

// streamGemini relays the canonical stream as Gemini alt=sse frames.
func (a *api) streamGemini(w http.ResponseWriter, r *http.Request, keyID string, req ir.Request, chain []routing.ChainEntry, traces ...*routing.Trace) {
	writeSSEHeaders(w)

	ew := geminiErrorWriter()
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

	enc := gemini.NewStreamEncoder(w)
	var terminalUsage *ir.Usage
	var streamErr *ir.UpstreamError
	var answer strings.Builder
	relayStream(ctx, ch, enc, &terminalUsage, &streamErr, req.Model, &answer)
	captureResponseText(ctx, req.Model, answer.String())

	var errMsg string
	if streamErr != nil {
		errMsg = redactUpstreamMessage(streamErr.Message)
	}
	status := http.StatusOK
	if gone, abandoned := abandonmentStatus(ctx); abandoned {
		status = gone
	} else {
		a.executor.ReportStreamOutcome(lastProvider(execTrace), streamErr)
	}
	a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace),
		status, time.Since(start).Milliseconds(), terminalUsage,
		routeTrace(append(traces, execTrace)...), errMsg)
}

// writeGeminiError emits the Gemini error shape:
// {"error":{"code":<http status>,"message":..,"status":<status enum>}}.
func writeGeminiError(w http.ResponseWriter, status int, errStatus, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	var body struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	body.Error.Code = status
	body.Error.Message = message
	body.Error.Status = errStatus
	raw, err := json.Marshal(body)
	if err != nil {
		raw = []byte(`{"error":{"code":500,"message":"internal error","status":"INTERNAL"}}`)
	}
	_, _ = w.Write(raw)
}

// geminiErrorWriter answers Gemini-shaped error bodies.
func geminiErrorWriter() errorWriter {
	return errorWriter{
		// The reference's retryable state reports 503 UNAVAILABLE (the
		// GenerateContentError enum's unavailable status).
		write:          writeGeminiError,
		overloadStatus: http.StatusServiceUnavailable,
		overloadType:   "UNAVAILABLE",
	}
}
