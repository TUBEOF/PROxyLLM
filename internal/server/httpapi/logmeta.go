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
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// Protocol surface identifiers stored in request_logs.protocol.
const (
	protocolAnthropic       = "anthropic"
	protocolOpenAI          = "openai"
	protocolOpenAIResponses = "openai-responses"
)

type logMeta struct {
	Protocol string // one of the protocol* constants
	Features provider.Capabilities
	ConvKey  string // deterministic conversation derivation input
	RunID    string // "" until beginRequest registered an in-flight entry

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
	return r.WithContext(context.WithValue(r.Context(), logMetaCtxKey{}, meta))
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
