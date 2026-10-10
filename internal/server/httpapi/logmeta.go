package httpapi

// logMeta is the per-request metadata writeRequestLog persists. It rides
// the request context (set once at each surface's decode site) so the ~20
// writeRequestLog call sites stay untouched — protocol, features, and the
// conversation key are known where the request body is, not where the log
// row is written.
//
// beginRequest recomputes requiredCapabilities although the run* handlers
// compute it again for ResolveChainFor: it is a pure, cheap function and
// duplicating the call keeps the run* signatures untouched — do NOT
// "deduplicate" this by threading meta through their params.

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/tubeof/proxyllm/internal/auth"
	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/reqlog"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// Protocol surface identifiers stored in request_logs.protocol.
const (
	protocolAnthropic       = "anthropic"
	protocolOpenAI          = "openai"
	protocolOpenAIResponses = "openai-responses"
)

// settingKeyCapturePayloads is the operator's switch for storing the
// conversation and the answer beside each log row (the storage package owns the
// key; the admin API reads the same constant).
const settingKeyCapturePayloads = storage.RequestPayloadLoggingKey

type logMeta struct {
	Protocol string // one of the protocol* constants
	Features provider.Capabilities
	ConvKey  string // deterministic conversation derivation input
	RunID    string // "" until beginRequest registered an in-flight entry

	// Payload capture (2026-10-10 Logs page): the conversation the request
	// carried and the answer it produced, held as IR values until the log row
	// is written. Holding the values — rather than encoded text — is the point:
	// encoding and redaction are real work, and they belong off the request path
	// (see capturePayload), where no client is waiting on them.
	//
	// Pointers to the setter's own copies, never to a caller's local: the
	// capture runs on its own goroutine, after the handler has returned.
	//
	// Either may be nil (capture off, or nothing to capture).
	capturedReq  *ir.Request
	capturedResp *ir.Response

	// finish deregisters the in-flight entry (nil without a Stream wired).
	// It lives here so writeRequestLog — every terminal path's single
	// choke point — owns the exactly-once handoff.
	finish func(row reqlog.LogRow)
}

type logMetaCtxKey struct{}

// marshalFeatures renders the derived capability set as the request_logs
// features JSON (booleans the WebUI drawer renders as badges).
func marshalFeatures(c provider.Capabilities) json.RawMessage {
	raw, err := json.Marshal(map[string]bool{
		"streaming":  c.Streaming,
		"tools":      c.Tools,
		"thinking":   c.Thinking,
		"web_search": c.WebSearch,
		"web_fetch":  c.WebFetch,
		"vision":     c.Vision,
	})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

func metaFromCtx(ctx context.Context) *logMeta {
	meta, _ := ctx.Value(logMetaCtxKey{}).(*logMeta)
	return meta
}

// beginRequest annotates the request with its log meta (protocol +
// content-derived features + conversation key) and registers the in-flight
// entry when a Stream is wired (nil keeps every existing test green). The
// returned request carries the updated context — handlers use it in place
// of r from this point on. raw is the undecoded wire body (the
// conversation hints metadata/previous_response_id ride it — the surface
// codecs have no IR field for them).
func (a *api) beginRequest(r *http.Request, protocol string, raw []byte, req ir.Request) *http.Request {
	meta := &logMeta{
		Protocol: protocol,
		Features: requiredCapabilities(req),
		ConvKey:  a.conversationKey(r, protocol, raw, req),
	}
	if a.stream != nil {
		in := reqlog.Inflight{
			ClientKeyID: auth.ClientKeyIDFromContext(r.Context()),
			Model:       req.Model,
			Protocol:    protocol,
		}
		var runID string
		meta.finish, runID = a.stream.Start(in)
		meta.RunID = runID
	}
	r = r.WithContext(context.WithValue(r.Context(), logMetaCtxKey{}, meta))
	// The conversation is known here and nowhere else as cheaply. Only the
	// reference is taken; encoding it is deferred to the payload write.
	a.captureRequest(r.Context(), req)
	return r
}

// finishInflight hands the terminal row to the in-flight tracker (never
// fails, never double-fires — Stream.finish is idempotent; nil-safe for
// requests without wired Stream or pre-registration rows).
func (a *api) finishInflight(meta *logMeta, row reqlog.LogRow) {
	if meta == nil || meta.finish == nil {
		return
	}
	meta.finish(row)
}
