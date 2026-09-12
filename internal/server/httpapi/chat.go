package httpapi

// POST /v1/chat/completions (plan §5.1, wire facts O2–O4, O10, O11): the
// OpenAI chat surface. The client body decodes through the chat codec into
// IR, routes, executes, and re-encodes into the chat wire — same-surface
// relay with the per-model compat policy applied first (§6.4).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/proxyllm/proxyllm/internal/auth"
	"github.com/proxyllm/proxyllm/internal/routing"
	"github.com/proxyllm/proxyllm/internal/translate"
	"github.com/proxyllm/proxyllm/internal/translate/ir"
	"github.com/proxyllm/proxyllm/internal/translate/openai"
)

// chatProbe is the raw-body probe for reasoning_effort (§6.4): the chat
// codec has no wire field for it (the chat surface never forwards effort
// hints upstream verbatim), but the compat policy must see the intent —
// a block/strip policy is decided by the client's thinking request, not
// by what the codec kept.
type chatProbe struct {
	ReasoningEffort string `json:"reasoning_effort"`
}

// handleChat serves POST /v1/chat/completions (streaming + non-streaming).
func (a *api) handleChat(w http.ResponseWriter, r *http.Request) {
	raw, ok := readBody(w, r)
	if !ok {
		return
	}

	// Decode through the surface codec.
	codec := openai.Codec{}
	req, err := codec.DecodeRequest(raw)
	if err != nil {
		a.writeRouteError(w, openAIErrorWriter(), err)
		return
	}

	// Probe the raw body for reasoning_effort: translate to the IR
	// Thinking intent so ApplyCompat governs it (§6.4).
	var probe chatProbe
	if json.Unmarshal(raw, &probe) == nil && probe.ReasoningEffort != "" {
		if req.Thinking == nil {
			req.Thinking = &ir.ThinkingConfig{Enabled: true}
		}
	}

	a.runChat(w, r, raw, req)
}

// runChat executes the shared pipeline for the chat surface.
func (a *api) runChat(w http.ResponseWriter, r *http.Request, raw []byte, req ir.Request) {
	ctx := r.Context()
	keyID := auth.ClientKeyIDFromContext(ctx)

	compat := a.compatFor(ctx, req.Model)
	req, compatTrace, err := routing.ApplyCompat(compat, req)
	if err != nil {
		// ErrThinkingBlocked → 400 O10 body + log row (never silent).
		_, _ = a.writeRouteError(w, openAIErrorWriter(), err)
		a.writeRequestLog(ctx, keyID, req.Model, "", http.StatusBadRequest, 0,
			nil, routeTrace(compatTrace), redactedMessage(err))
		return
	}

	chain, routerTrace, err := a.router.ResolveChainFor(req.Model,
		requiredCapabilities(req), routing.WithKeyID(keyID))
	if err != nil {
		_, _ = a.writeRouteError(w, openAIErrorWriter(), err)
		a.writeRequestLog(ctx, keyID, req.Model, "", mapRouteErrorStatus(err), 0,
			nil, routeTrace(compatTrace, routerTrace), redactedMessage(err))
		return
	}

	if req.Stream {
		a.streamChat(w, r, keyID, req, chain, compatTrace, routerTrace)
		return
	}

	start := time.Now()
	resp, execTrace, err := a.executor.Execute(ctx, chain, req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		// Executor failures carry their trace — log the row from it.
		_, msg := a.writeRouteError(w, openAIErrorWriter(), err)
		a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace),
			mapRouteErrorStatus(err), latency, nil,
			routeTrace(compatTrace, routerTrace, execTrace), msg)
		return
	}

	// OP-2: echo the requested model name (see messages.go).
	resp.Model = req.Model
	out, err := openai.Codec{}.EncodeResponse(resp)
	if err != nil {
		_, msg := a.writeRouteError(w, openAIErrorWriter(), err)
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

// streamChat relays an upstream stream to the chat-wire client (O4:
// data-only SSE frames, [DONE] terminal, per-event flush).
func (a *api) streamChat(w http.ResponseWriter, r *http.Request, keyID string, req ir.Request, chain []routing.ChainEntry, traces ...*routing.Trace) {
	ctx := r.Context()

	start := time.Now()
	ch, cancel, execTrace, err := a.executor.ExecuteStream(ctx, chain, req)
	if err != nil {
		// Pre-stream failure: a normal HTTP error is still possible.
		_, msg := a.writeRouteError(w, openAIErrorWriter(), err)
		a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace),
			mapRouteErrorStatus(err), time.Since(start).Milliseconds(), nil,
			routeTrace(append(traces, execTrace)...), msg)
		return
	}
	defer cancel()

	enc := openai.NewStreamEncoder(w)
	var terminalUsage *ir.Usage
	var streamErr *ir.UpstreamError
	relayStream(ctx, ch, enc, &terminalUsage, &streamErr, req.Model)

	latency := time.Since(start).Milliseconds()
	status := http.StatusOK
	var errMsg string
	if streamErr != nil {
		errMsg = streamErr.Message
	}
	var usageArg *ir.Usage
	if terminalUsage != nil {
		usageArg = terminalUsage
	}
	a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace),
		status, latency, usageArg,
		routeTrace(append(traces, execTrace)...), errMsg)
}

// readBody reads the request body with the shared limits. One extra byte
// is read past the cap so truncation is detected and reported as 413 —
// a silently truncated body would otherwise surface as a confusing JSON
// decode error (Analysis G input validation).
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, errTypeInvalidRequest, "unable to read request body")
		return nil, false
	}
	if len(raw) > maxBodyBytes {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, errTypeInvalidRequest,
			"request body exceeds the 32MB limit")
		return nil, false
	}
	return raw, true
}

// maxBodyBytes caps request bodies (Analysis G input validation).
const maxBodyBytes = 32 << 20

// relayStream forwards canonical events to the client encoder until the
// upstream channel closes, capturing the terminal usage and any terminal
// error for the request-log row. Mid-stream errors keep HTTP 200 (the SSE
// stream already started) and terminate with a protocol-conform error
// event; an empty error type is normalized (the transport pump reports
// body-read failures with the message only). The Start event's model is
// rewritten to clientModel: the client's requested model name is echoed
// (OP-2 alias routing), never the upstream-internal name.
func relayStream(ctx context.Context, ch <-chan ir.StreamEvent, enc translate.StreamEncoder, terminalUsage **ir.Usage, streamErr **ir.UpstreamError, clientModel string) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Type == ir.Start {
				if ev.Response == nil {
					ev.Response = &ir.Response{}
				}
				ev.Response.Model = clientModel
			}
			if ev.Type == ir.Error {
				if ev.Error != nil {
					if ev.Error.Type == "" {
						ev.Error.Type = errTypeUpstream
					}
					*streamErr = ev.Error
				}
				// B2: the error event is terminal (A13/O7) — nothing may
				// follow it on the wire, so stop relaying even if a
				// misbehaving provider kept the channel open.
				_ = enc.Write(ctx, ev)
				return
			}
			if ev.Type == ir.MessageDelta && ev.MessageDelta != nil {
				u := ev.MessageDelta.Usage
				*terminalUsage = &u
			}
			// B1: a failed client write (disconnect) must stop the pump —
			// keep relaying only while the client is actually receiving.
			if enc.Write(ctx, ev) != nil {
				return
			}
		}
	}
}
