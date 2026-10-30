package httpapi

// Native Ollama client surface: POST /api/chat (JSON or NDJSON streaming)
// and GET /api/tags. The same IR pipeline serves it; only the wire codec and
// error shape differ.

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/tubeof/proxyllm/internal/auth"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/ollama"
)

// ollamaErrorWriter answers Ollama's {"error":"..."} shape.
func ollamaErrorWriter() errorWriter {
	return errorWriter{
		write:          writeSystemOneError,
		overloadStatus: http.StatusServiceUnavailable,
	}
}

// handleOllamaChat serves POST /api/chat.
func (a *api) handleOllamaChat(w http.ResponseWriter, r *http.Request) {
	ew := ollamaErrorWriter()

	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	req, err := ollama.Codec{}.DecodeRequest(raw)
	if err != nil {
		status, msg := a.writeDecodeError(w, ew, err)
		a.writeRequestLog(r.Context(), auth.ClientKeyIDFromContext(r.Context()),
			"", "", status, 0, nil, nil, msg)
		return
	}

	r = a.beginRequest(r, protocolOllama, raw, req)
	ctx := r.Context()
	keyID := auth.ClientKeyIDFromContext(ctx)

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
		a.streamOllama(w, r, keyID, req, chain, compatTrace, routerTrace, resolveTrace)
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
	out, err := ollama.Codec{}.EncodeResponse(resp)
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

// streamOllama relays the canonical stream as Ollama NDJSON lines.
func (a *api) streamOllama(w http.ResponseWriter, r *http.Request, keyID string, req ir.Request, chain []routing.ChainEntry, traces ...*routing.Trace) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")

	ew := ollamaErrorWriter()
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

	enc := ollama.NewStreamEncoder(w)
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

// handleOllamaTags serves GET /api/tags with the local model rows.
func (a *api) handleOllamaTags(w http.ResponseWriter, r *http.Request) {
	models, err := a.store.ListModels(r.Context())
	if err != nil {
		writeSystemOneError(w, http.StatusInternalServerError, "", "model list unavailable")
		return
	}
	type tag struct {
		Name  string `json:"name"`
		Model string `json:"model"`
	}
	out := struct {
		Models []tag `json:"models"`
	}{Models: make([]tag, 0, len(models))}
	for _, m := range models {
		out.Models = append(out.Models, tag{Name: m.ID, Model: m.ID})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
