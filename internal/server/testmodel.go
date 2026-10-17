package server

// Model test (Models page): one minimal "Sag OK" round trip
// against the model row's provider account — the operator-facing proof
// that an account actually serves the model. No routing, no executor, no
// compatibility policy: the raw provider.Complete with the row's wire
// name, a tiny budget, and a hard timeout. Failures are plain-language
// and redacted (the operator needs a reason, never a raw upstream body).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tubeof/proxyllm/internal/observability"
	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/server/webui"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// testModelTimeout bounds the whole test round trip — a hung upstream must
// not pin the operator's browser tab or a server worker.
const testModelTimeout = 15 * time.Second

// testModelMaxTokens bounds the test reply: a minimal acknowledgment must
// not cost real money on a verbose model.
const testModelMaxTokens = 16

// testModel returns the AdminParams.TestModel implementation: resolve the
// model row, look up its provider account in the live registry, and fire
// one minimal complete. The registry closure keeps provider reloads live —
// the test always hits the current account set.
func (s *Server) testModel(reg provider.Registry) func(context.Context, string) (webui.TestModelResult, error) {
	return func(ctx context.Context, modelID string) (webui.TestModelResult, error) {
		ctx, cancel := context.WithTimeout(ctx, testModelTimeout)
		defer cancel()

		row, err := s.store.GetModel(ctx, modelID)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return webui.TestModelResult{OK: false, Error: "unknown model"}, nil
			}
			return webui.TestModelResult{}, err
		}

		p, ok := reg.Get(row.ProviderID)
		if !ok {
			return webui.TestModelResult{OK: false, Error: "provider account is not registered (no credential? restart or re-save the account)"}, nil
		}

		req := ir.Request{
			Model: row.Name,
			// The German minimal prompt is the user's chosen test phrase:
			// a fixed, tiny ask — no thinking, no tools, no stream.
			MaxTokens: testModelMaxTokens,
			Messages:  []ir.Message{{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "Sag OK"}}}},
		}

		// The exact wire body, when the provider can encode one: the popup
		// shows what went out, and the wire shape differs per provider
		// (chat-completions vs Responses). A provider without the surface,
		// or a failed encode, simply leaves the field empty — a missing
		// diagnostic must never fail the test itself.
		preview := ""
		if prev, ok := p.(provider.RequestPreviewer); ok {
			if raw, err := prev.PreviewRequest(req); err == nil {
				preview = redactor.Redact(truncateTestPreview(string(raw)))
			}
		}

		started := time.Now()
		resp, err := p.Complete(ctx, req)
		latency := time.Since(started).Milliseconds()
		if err != nil {
			return webui.TestModelResult{
				OK: false, LatencyMS: latency, Error: plainUpstreamCause(err),
				Request: preview, Detail: failureDetail(err),
			}, nil
		}
		// The reply travels to the browser too — the full redactor applies
		// (a misbehaving upstream could echo secret-shaped text in even a
		// 16-token reply).
		return webui.TestModelResult{
			OK: true, LatencyMS: latency, Request: preview,
			Reply: redactor.Redact(firstText(resp)),
		}, nil
	}
}

// redactor is the single redaction gate for everything that leaves the
// test endpoint toward the browser — the same engine the log handler uses
// (URL query secrets, JWTs, JSON-shaped credentials, oversized blobs), not
// a hand-rolled prefix list.
var redactor = observability.NewRedactor()

// firstText extracts the first textual part of the response — the minimal
// acknowledgment the operator expects to see.
func firstText(resp ir.Response) string {
	for _, part := range resp.Content {
		if t, ok := part.(ir.TextPart); ok && t.Text != "" {
			return t.Text
		}
	}
	return ""
}

// plainUpstreamCause renders a failure cause the operator can act on,
// fully redacted (this string travels to the browser, not just logs — the
// full redactor covers URL query-param secrets, JWTs, and JSON-shaped
// credentials the prefix list would miss).
func plainUpstreamCause(err error) string {
	var up *provider.UpstreamError
	if errors.As(err, &up) {
		status := ""
		if up.HTTPStatus > 0 {
			status = fmt.Sprintf(" (HTTP %d)", up.HTTPStatus)
		}
		return "upstream rejected the test request" + status + ": " + redactor.Redact(up.Message)
	}
	return "request failed before the upstream answered: " + redactor.Redact(err.Error())
}

// failureDetail renders the structured half of a failed round trip for the
// popup: the upstream's status and decoded error object, redacted like
// everything else that leaves this endpoint. A failure that never reached a
// response (transport, timeout, cancellation) keeps HTTPStatus 0 — the
// honest "no answer arrived" rather than a fabricated one.
func failureDetail(err error) *webui.TestFailureDetail {
	var up *provider.UpstreamError
	if !errors.As(err, &up) {
		return &webui.TestFailureDetail{Message: redactor.Redact(err.Error())}
	}
	message := up.Message
	if message == "" {
		// The body carried no decodable error object: Error() renders the
		// generic wording, which is all there is to say.
		message = up.Error()
	}
	return &webui.TestFailureDetail{
		HTTPStatus: up.HTTPStatus,
		Type:       up.Type,
		Code:       up.Code,
		Message:    redactor.Redact(message),
	}
}

// testPreviewMaxBytes bounds the request preview that travels to the
// browser. The test request is a 16-token ask, so this is a ceiling on
// pathological encoders, not a working limit.
const testPreviewMaxBytes = 8 << 10

// truncateTestPreview bounds the preview and marks the cut, so a shortened
// body is never read as the whole request.
func truncateTestPreview(s string) string {
	if len(s) <= testPreviewMaxBytes {
		return s
	}
	return s[:testPreviewMaxBytes] + "\n… (truncated)"
}
