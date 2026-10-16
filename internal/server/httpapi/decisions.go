package httpapi

// Decision surfaces (decision models — System One / Jev and the OpenAI
// Decisions API): POST /v1/systemone and POST /v1/decisions share one
// pipeline — decode through the surface's decision codec, route by model
// with the `decision` capability required, dispatch through the executor's
// decision walk, encode through the same codec. Either surface reaches
// either upstream through the IR: the translation is the codecs' job.
// Both are non-streaming by contract (neither reference offers streaming),
// so the handlers have no stream branch.

import (
	"net/http"
	"strconv"
	"time"

	"github.com/tubeof/proxyllm/internal/auth"
	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/openai"
	"github.com/tubeof/proxyllm/internal/translate/systemone"
)

// decisionSurface bundles one client-facing decision surface: its codec
// pair, its protocol value for the log row, and its error envelope.
type decisionSurface struct {
	protocol string
	ew       errorWriter
	decode   func([]byte) (ir.DecisionRequest, error)
	encode   func(ir.DecisionResponse) ([]byte, error)
}

func systemOneSurface() decisionSurface {
	return decisionSurface{
		protocol: protocolSystemOne,
		ew:       systemOneErrorWriter(),
		decode:   systemone.Codec{}.DecodeRequest,
		encode:   systemone.Codec{}.EncodeResponse,
	}
}

func decisionsSurface() decisionSurface {
	return decisionSurface{
		protocol: protocolDecisions,
		ew:       openAIErrorWriter(),
		decode:   openai.DecisionsCodec{}.DecodeRequest,
		encode:   openai.DecisionsCodec{}.EncodeResponse,
	}
}

// handleSystemOne serves POST /v1/systemone (the System One / Jev shape).
func (a *api) handleSystemOne(w http.ResponseWriter, r *http.Request) {
	a.handleDecision(w, r, systemOneSurface())
}

// handleDecisions serves POST /v1/decisions (the OpenAI Decisions shape).
func (a *api) handleDecisions(w http.ResponseWriter, r *http.Request) {
	a.handleDecision(w, r, decisionsSurface())
}

// handleDecision is the shared pipeline. There is no compat step (decision
// requests carry no thinking flags, tools or cache markers) and no
// streaming branch (non-streaming by contract on both references).
func (a *api) handleDecision(w http.ResponseWriter, r *http.Request, s decisionSurface) {
	raw, ok := readBody(w, r)
	if !ok {
		return
	}

	req, err := s.decode(raw)
	if err != nil {
		// A decode or validation failure is the client's: 400 in the
		// surface's envelope, observable as a log row like every request.
		status, msg := a.writeDecisionError(w, s.ew, err)
		a.writeRequestLog(r.Context(), auth.ClientKeyIDFromContext(r.Context()),
			"", "", status, 0, nil, nil, msg)
		return
	}

	r = a.beginDecisionRequest(r, s.protocol, req.Model)

	ctx := r.Context()
	keyID := auth.ClientKeyIDFromContext(ctx)

	// The `decision` capability is the routing gate: a chain entry whose
	// provider does not declare it is filtered before dispatch (the
	// executor's walk repeats the check at runtime via provider.Decider).
	chain, routerTrace, err := a.router.ResolveChainFor(req.Model, provider.Capabilities{Decision: true},
		routing.WithKeyID(keyID), routing.WithRequestContext(ctx))
	if err != nil {
		_, msg := a.writeRouteError(w, s.ew, err)
		a.writeRequestLog(ctx, keyID, req.Model, "", s.ew.errorStatus(err), 0,
			nil, routeTrace(routerTrace), msg)
		return
	}

	// Model identity: the entries' routing ID resolves to the wire name
	// (the chat surfaces' rule — rows win, the tail after the last slash
	// is the fallback).
	resolveTrace := &routing.Trace{}
	chain = a.resolveChainWireModels(ctx, chain, resolveTrace)

	start := time.Now()
	resp, execTrace, err := a.executor.ExecuteDecision(ctx, chain, req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		_, msg := a.writeRouteError(w, s.ew, err)
		a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace),
			s.ew.errorStatus(err), latency, nil,
			routeTrace(routerTrace, resolveTrace, execTrace), msg)
		return
	}

	// A refusal is an answer shape on the OpenAI Decisions surface but has
	// none on systemone: there it fails loudly (502) instead of being
	// invented into an answer. The decisions surface relays it as the
	// `refusal` entry it is.
	if s.protocol == protocolSystemOne {
		if name, refused := refusalAnswer(resp); refused {
			status := http.StatusBadGateway
			msg := "the upstream model refused to answer question " + strconv.Quote(name)
			s.ew.write(w, status, errTypeUpstream, msg)
			a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace), status, latency,
				resp.Usage, routeTrace(routerTrace, resolveTrace, execTrace), msg)
			return
		}
	}

	// Echo the requested model name (the chat surfaces' rule): the client
	// sees the ID it asked for, not the upstream's wire name.
	resp.Model = req.Model
	out, err := s.encode(resp)
	if err != nil {
		status := http.StatusInternalServerError
		msg := redactUpstreamMessage(err.Error())
		s.ew.write(w, status, errTypeUpstream, msg)
		a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace), status, latency,
			resp.Usage, routeTrace(routerTrace, resolveTrace, execTrace), msg)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)

	a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace),
		http.StatusOK, latency, resp.Usage,
		routeTrace(routerTrace, resolveTrace, execTrace), "")
}

// writeDecisionError answers a decode or validation failure with 400 in the
// surface's envelope.
func (a *api) writeDecisionError(w http.ResponseWriter, ew errorWriter, err error) (int, string) {
	msg := redactUpstreamMessage(err.Error())
	ew.write(w, http.StatusBadRequest, errTypeInvalidRequest, msg)
	return http.StatusBadRequest, msg
}

// refusalAnswer names the first refusal answer in the response, if any.
func refusalAnswer(resp ir.DecisionResponse) (string, bool) {
	for _, ans := range resp.Answers {
		if ans.Type == ir.DecisionRefusal {
			return ans.Name, true
		}
	}
	return "", false
}
