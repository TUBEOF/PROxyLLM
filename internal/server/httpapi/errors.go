package httpapi

// Dual-surface error model: protocol-conform bodies on both surfaces —
// the OpenAI error shape on the OpenAI-family surfaces and the Anthropic
// error envelope on the messages surface — with
// upstream status codes preserved where they exist and never a bare Go
// error string. Upstream credentials are never echoed: error bodies are
// assembled from the mapped UpstreamError fields only (the transport never
// copies request headers into errors).

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
)

// writeSSEHeaders declares a response as an event stream, before any event
// is encoded. An SSE response MUST carry Content-Type: text/event-stream: a
// strict client (a browser's native EventSource, and any spec-conform SSE
// consumer) FAILS the connection on any other type, turning a streaming
// response into a silently dead one. The cache guards accompany it — an
// intermediary that buffers frames defeats streaming — with
// X-Accel-Buffering disabling the same buffering on nginx-fronted
// deployments (ignored where it does not apply).
//
// Sets no status: a pre-stream failure must still be able to answer with a
// real HTTP error, and these headers are harmless on one. Called from the
// streaming entry points, so the headers are on the wire before the first
// event is flushed.
func writeSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
}

// errorType codes the surfaces emit for internal (non-upstream) failures.
const (
	errTypeInvalidRequest = "invalid_request_error"
	errTypeNotFound       = "not_found_error"
	errTypeUpstream       = "upstream_error"
)

// writeOpenAIError emits the OpenAI error shape:
// {"error":{"message":..,"type":..,"code":null,"param":null}}.
func writeOpenAIError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSONErrorBody(w, errType, message, errType)
}

// writeJSONErrorBody writes the OpenAI error object with code mirroring the type
// (internal errors carry no distinct machine code; upstream body codes are
// relayed by writeUpstreamError below).
func writeJSONErrorBody(w http.ResponseWriter, errType, message, code string) {
	var body openAIErrorEnvelope
	body.Error.Message = message
	body.Error.Type = errType
	body.Error.Code = code
	raw, err := json.Marshal(body)
	if err != nil {
		raw = []byte(`{"error":{"message":"internal error","type":"server_error","code":"server_error","param":null}}`)
	}
	_, _ = w.Write(raw)
}

// openAIErrorEnvelope is marshalled with encoding/json rather than built by
// string concatenation: the values are JSON-escaped by the encoder, and the
// shape is obvious to readers and scanners.
type openAIErrorEnvelope struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Code    string  `json:"code"`
		Param   *string `json:"param"`
	} `json:"error"`
}

// writeAnthropicError emits the Anthropic error envelope:
// {"type":"error","error":{"type":..,"message":..}}.
func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	var body anthropicErrorEnvelope
	body.Type = "error"
	body.Error.Type = errType
	body.Error.Message = message
	raw, err := json.Marshal(body)
	if err != nil {
		raw = []byte(`{"type":"error","error":{"type":"api_error","message":"internal error"}}`)
	}
	_, _ = w.Write(raw)
}

type anthropicErrorEnvelope struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// writeSystemOneError emits the systemone error shape: {"error":".."} with
// the documented status codes (400 invalid request, 401 auth, 402 cloud
// plan/credits, 404 model, 413 limits, 500 scoring failure). The type field
// has no counterpart on this wire.
func writeSystemOneError(w http.ResponseWriter, status int, _ string, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	raw, err := json.Marshal(struct {
		Error string `json:"error"`
	}{Error: message})
	if err != nil {
		raw = []byte(`{"error":"internal error"}`)
	}
	_, _ = w.Write(raw)
}

// errorWriter is the surface-specific error emitter pair.
type errorWriter struct {
	write func(w http.ResponseWriter, status int, errType, message string)

	// overloadStatus is the surface's retryable-unavailability mapping
	// for routing.ErrChainOverloaded (every chain provider in health
	// cooldown): 529/overloaded_error on the Anthropic surface,
	// 503 on the OpenAI surfaces (docs/api/errors.md (error model) —
	// "upstream overload maps to 529 semantics (overloaded_error)").
	// Zero means the surface has no overload mapping (unused).
	overloadStatus int
	overloadType   string
}

// openAIErrorWriter answers OpenAI-shaped error bodies.
func openAIErrorWriter() errorWriter {
	return errorWriter{
		write:          writeOpenAIError,
		overloadStatus: http.StatusServiceUnavailable,
		overloadType:   "server_error",
	}
}

// anthropicErrorWriter answers Anthropic-shaped error envelopes.
func anthropicErrorWriter() errorWriter {
	return errorWriter{
		write:          writeAnthropicError,
		overloadStatus: 529, // Anthropic overload semantics, not a net/http constant
		overloadType:   "overloaded_error",
	}
}

// systemOneErrorWriter answers systemone-shaped error bodies. The
// reference's retryable state is 500 ("model loading, rendering, or
// scoring failed"), so an all-providers-in-cooldown chain maps there.
func systemOneErrorWriter() errorWriter {
	return errorWriter{
		write:          writeSystemOneError,
		overloadStatus: http.StatusInternalServerError,
		overloadType:   "",
	}
}

// writeRouteError maps a pipeline error to the wire:
//
//   - routing.ErrThinkingBlocked → 400, protocol-conform rejection
//   - routing.ErrNoRoute → 404, the model has no route
//   - routing.ErrChainOverloaded → the surface's retryable overload
//     mapping (529 overloaded_error on Anthropic, 503 on OpenAI)
//   - upstream errors keep their status (429 stays 429); a chain whose every
//     attempt died before an HTTP response is 502, or 504 when one ran out of
//     time; exhausted chains otherwise fall back to the last upstream
//     classification, else 500
//
// It returns the status written (for the request-log row) and the
// redacted message.
func (a *api) writeRouteError(w http.ResponseWriter, ew errorWriter, err error) (int, string) {
	if errors.Is(err, routing.ErrChainOverloaded) && ew.overloadStatus > 0 {
		msg, retryAfter := overloadMessage(err)
		if retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		}
		ew.write(w, ew.overloadStatus, ew.overloadType, msg)
		return ew.overloadStatus, msg
	}
	status, errType, message := mapRouteError(err)
	ew.write(w, status, errType, message)
	return status, message
}

// overloadMessage names the cooldown the router reported — circuit breaker
// or an upstream quota/rate-limit window — and, when a deadline is known, the
// seconds until the earliest provider recovers (returned for Retry-After).
// The sentinel-only fallback keeps the historical wording for callers that
// did not carry the typed facts (decision surfaces).
func overloadMessage(err error) (string, int) {
	const breaker = "All providers for this model are in health cooldown (circuit breaker open)."
	var oe *routing.ChainOverloadedError
	if !errors.As(err, &oe) {
		return breaker + " Retry shortly.", 0
	}
	secs := 0
	if !oe.Until.IsZero() {
		secs = int(math.Ceil(time.Until(oe.Until).Seconds()))
		if secs < 1 {
			secs = 1
		}
	}
	msg := breaker
	if oe.Reason == "quota_exhausted" {
		msg = "All providers for this model are in an upstream quota/rate-limit cooldown."
	}
	if secs > 0 {
		return msg + " Retry in " + strconv.Itoa(secs) + "s.", secs
	}
	return msg + " Retry shortly.", 0
}

// writeDecodeError answers a request body the surface codec could not decode.
// A malformed request is the client's, never a 500: docs/api/errors.md lists
// body decode failure as 400. The message is redacted like every logged text
// (a type/path error normally carries no values, but the rule is blanket).
func (a *api) writeDecodeError(w http.ResponseWriter, ew errorWriter, err error) (int, string) {
	msg := redactUpstreamMessage(err.Error())
	ew.write(w, http.StatusBadRequest, errTypeInvalidRequest, msg)
	return http.StatusBadRequest, msg
}

// errorStatus maps a pipeline error to its HTTP status without writing
// the body (the request-log row needs the status even when the response
// was already served). Mirrors writeRouteError, surface-aware for the
// overload mapping so the log row never disagrees with the wire status.
func (ew errorWriter) errorStatus(err error) int {
	if errors.Is(err, routing.ErrChainOverloaded) && ew.overloadStatus > 0 {
		return ew.overloadStatus
	}
	return mapRouteErrorStatus(err)
}

// mapRouteErrorStatus maps a pipeline error to its HTTP status without
// writing the body (the request-log row needs the status even when the
// response was already served).
func mapRouteErrorStatus(err error) int {
	status, _, _ := mapRouteError(err)
	return status
}

// mapRouteError centralizes the status/type/message mapping shared by the
// non-streaming and streaming error paths.
func mapRouteError(err error) (int, string, string) {
	switch {
	case errors.Is(err, ErrNonClaudeBlocked):
		return http.StatusBadRequest, errTypeInvalidRequest,
			"Non-Claude models are blocked on the Anthropic surface (anthropic_surface_non_claude=block)."
	case errors.Is(err, routing.ErrThinkingBlocked):
		return http.StatusBadRequest, errTypeInvalidRequest,
			"This model's compatibility policy does not allow thinking requests (thinking_handling=block)."
	case errors.Is(err, routing.ErrEmptyRequest):
		return http.StatusBadRequest, errTypeInvalidRequest,
			"The request contains no messages; an upstream chat API requires at least one."
	case errors.Is(err, routing.ErrNoRoute):
		// The reference's spelling for an unknown model — "model: <id>" — is
		// load-bearing on the wire: Claude Code classifies a 404 as
		// model-not-found, the precondition for its own model fallback, only
		// when the body's error type is not_found_error AND the message
		// contains "model:". A prose message reads as an opaque 404 and the
		// client cannot recover.
		message := "model: (unknown)"
		var nre *routing.NoRouteError
		if errors.As(err, &nre) && nre.Model != "" {
			// The name is client input and rides BOTH the wire and the
			// request-log row (writeRouteError's msg) — credential shapes are
			// masked out of it like everywhere else (ErrorRedacted contract).
			message = "model: " + redactUpstreamMessage(nre.Model)
		}
		return http.StatusNotFound, errTypeNotFound, message
	}
	// A request that ended because its context did is not a routing failure:
	// the client stopped reading, or a budget of ours ran out. The status is the
	// log marker for which of the two (see abandonment.go) — a 499 never reaches
	// a client, and a 504 is the honest answer when it is our own deadline.
	if status, abandoned := mapAbandonment(err); abandoned {
		if status == statusClientClosedRequest {
			return status, errTypeUpstream, "The client closed the request before an answer was complete."
		}
		return status, errTypeUpstream, "The request exceeded the time allowed for it."
	}
	// Chain exhausted: the last attempt's status is the right answer only when
	// there was one kind of failure — a single-provider chain's 429 must still
	// read as 429 so the client backs off. A genuine mix has no single target to
	// blame, and reporting the last one sends the client to the wrong place; the
	// gateway is what could not deliver, and 502 says so (504 when a target ran
	// out of time).
	if mixed, timedOut, ok := routing.ChainFailure(err); ok && mixed {
		if timedOut {
			return statusAbandonedBudget, errTypeUpstream,
				"Every route for this request failed; at least one ran out of time."
		}
		return http.StatusBadGateway, errTypeUpstream,
			"Every route for this request failed, for different reasons."
	}

	// A homogeneous chain that never produced an HTTP response is a gateway
	// failure, not an internal error: 502 when the connection failed, 504 when
	// an attempt ran out of time (the response-header wall included). Before
	// this branch, a raw transport error fell through to 500, contradicting
	// docs/api/errors.md and hiding a retryable condition.
	if timedOut, ok := routing.ChainTransportFailure(err); ok {
		if timedOut {
			return statusAbandonedBudget, errTypeUpstream,
				"Every route for this request failed; at least one ran out of time."
		}
		return http.StatusBadGateway, errTypeUpstream,
			"Every route for this request failed; no provider produced a response."
	}

	// Upstream-classified errors: keep the status (429 stays 429), relay
	// the typed body fields — never a bare Go error string, never the
	// upstream credential (UpstreamError.Message never contains headers).
	if status, bodyCode, ok := routing.AsUpstreamError(err); ok {
		errType, message := upstreamErrorFields(err)
		if status <= 0 {
			// Transport failure (no HTTP response): bad gateway.
			status = http.StatusBadGateway
		}
		_ = bodyCode // status mapping only; the message carries the detail
		return status, errType, message
	}
	// Chain-exhausted with an unmappable last error or a decode failure.
	return http.StatusInternalServerError, errTypeUpstream, redactedMessage(err)
}

// upstreamErrorFields extracts the wire type + message from the
// upstream error chain, defaulting the type for transport failures that
// carry none.
func upstreamErrorFields(err error) (string, string) {
	var up *provider.UpstreamError
	if !errors.As(err, &up) {
		return errTypeUpstream, redactedMessage(err)
	}
	errType := up.Type
	if errType == "" {
		errType = errTypeUpstream
	}
	message := up.Message
	if message == "" {
		message = redactedMessage(err)
	}
	return errType, message
}

// redactedMessage renders err.Error() for the log/error surfaces minus
// well-known credential shapes. The executor errors embed upstream
// messages only, but defense in depth: mask token-shaped substrings
// before any storage or echo.
func redactedMessage(err error) string {
	return redactUpstreamMessage(err.Error())
}

// redactUpstreamMessage masks credential shapes in a raw upstream error
// message before it reaches request_logs' ErrorRedacted column. Mid-stream
// failures carry the provider's message verbatim (ir.UpstreamError.Message
// never contains headers), but defense in depth: the same masking rules as
// redactedMessage apply — a misbehaving upstream echoing a bearer token or
// key-shaped substring must never persist raw.
func redactUpstreamMessage(msg string) string {
	// The full redactor (Bearer, sk-/fc-/pk_ keys, JWTs, cookies, long
	// base64), not just the narrow prefix mask: this string is persisted in
	// request_logs.error_redacted and echoed to the client.
	return childRedactor.Redact(msg)
}
