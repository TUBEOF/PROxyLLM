package httpapi

// Dual-surface error model (plan §5.3): protocol-conform bodies on both
// surfaces — the OpenAI error shape (O10) on the OpenAI-family surfaces
// and the Anthropic error envelope (A13) on the messages surface — with
// upstream status codes preserved where they exist and never a bare Go
// error string. Upstream credentials are never echoed: error bodies are
// assembled from the mapped UpstreamError fields only (the transport never
// copies request headers into errors).

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/proxyllm/proxyllm/internal/provider"
	"github.com/proxyllm/proxyllm/internal/routing"
)

// errorType codes the surfaces emit for internal (non-upstream) failures.
const (
	errTypeInvalidRequest = "invalid_request_error"
	errTypeNotFound       = "not_found_error"
	errTypeUpstream       = "upstream_error"
)

// writeOpenAIError emits the O10 error shape:
// {"error":{"message":..,"type":..,"code":null,"param":null}}.
func writeOpenAIError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSONErrorBody(w, errType, message, errType)
}

// writeJSONErrorBody writes the O10 object with code mirroring the type
// (internal errors carry no distinct machine code; upstream body codes are
// relayed by writeUpstreamError below).
func writeJSONErrorBody(w http.ResponseWriter, errType, message, code string) {
	body := `{"error":{"message":` + jsonString(message) +
		`,"type":` + jsonString(errType) +
		`,"code":` + jsonString(code) +
		`,"param":null}}`
	_, _ = w.Write([]byte(body))
}

// writeAnthropicError emits the A13 error envelope:
// {"type":"error","error":{"type":..,"message":..}}.
func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := `{"type":"error","error":{"type":` + jsonString(errType) +
		`,"message":` + jsonString(message) + `}}`
	_, _ = w.Write([]byte(body))
}

// jsonString marshals s as a JSON string, falling back to "" on the
// impossible marshal failure of a string.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// errorWriter is the surface-specific error emitter pair.
type errorWriter struct {
	write func(w http.ResponseWriter, status int, errType, message string)
}

// openAIErrorWriter answers O10-shaped bodies.
func openAIErrorWriter() errorWriter {
	return errorWriter{write: writeOpenAIError}
}

// anthropicErrorWriter answers A13-shaped envelopes.
func anthropicErrorWriter() errorWriter {
	return errorWriter{write: writeAnthropicError}
}

// writeRouteError maps a pipeline error to the wire per §5.3:
//
//   - routing.ErrThinkingBlocked → 400, protocol-conform rejection
//   - routing.ErrNoRoute → 404, the model has no route
//   - upstream errors keep their status (429 stays 429 O10); transport
//     failures (status 0) become 502 bad gateway; exhausted chains fall
//     back to the last upstream classification, else 500
//
// It returns the status written (for the request-log row) and the
// redacted message.
func (a *api) writeRouteError(w http.ResponseWriter, ew errorWriter, err error) (int, string) {
	status, errType, message := mapRouteError(err)
	ew.write(w, status, errType, message)
	return status, message
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
	case errors.Is(err, routing.ErrNoRoute):
		return http.StatusNotFound, errTypeNotFound,
			"No route for the requested model. Add a routing rule or check the model name."
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

// upstreamErrorFields extracts the O10/A13 type + message from the
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
	msg := err.Error()
	msg = maskAfter(msg, "Bearer ")
	msg = maskKeyPrefixes(msg)
	return msg
}

// maskAfter replaces the substring following marker up to the next space
// or end (Authorization header values in accidentally-included messages).
func maskAfter(msg, marker string) string {
	idx := strings.Index(msg, marker)
	if idx < 0 {
		return msg
	}
	start := idx + len(marker)
	end := start
	for end < len(msg) && msg[end] != ' ' {
		end++
	}
	return msg[:start] + "[REDACTED]" + msg[end:]
}

// maskKeyPrefixes masks common credential prefixes appearing inline.
func maskKeyPrefixes(msg string) string {
	for _, prefix := range []string{"sk-", "fc-", "pk_"} {
		for {
			idx := strings.Index(msg, prefix)
			if idx < 0 {
				break
			}
			end := idx
			for end < len(msg) && msg[end] != ' ' && msg[end] != '"' && msg[end] != '\'' {
				end++
			}
			msg = msg[:idx] + "[REDACTED]" + msg[end:]
		}
	}
	return msg
}
