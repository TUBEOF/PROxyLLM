package server

// Model test (2026-09-22 Models page): one minimal "Sag OK" round trip
// against the model row's provider account — the operator-facing proof
// that an account actually serves the model. No routing, no executor, no
// compatibility policy: the raw provider.Complete with the row's wire
// name, a tiny budget, and a hard timeout. Failures are plain-language
// and redacted (the operator needs a reason, never a raw upstream body).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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

		started := time.Now()
		resp, err := p.Complete(ctx, req)
		latency := time.Since(started).Milliseconds()
		if err != nil {
			return webui.TestModelResult{OK: false, LatencyMS: latency, Error: plainUpstreamCause(err)}, nil
		}
		return webui.TestModelResult{OK: true, LatencyMS: latency, Reply: firstText(resp)}, nil
	}
}

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
// masking credential-shaped substrings (defense in depth: the log handler
// redacts too, but this string travels to the browser, not just logs).
func plainUpstreamCause(err error) string {
	var up *provider.UpstreamError
	if errors.As(err, &up) {
		status := ""
		if up.HTTPStatus > 0 {
			status = fmt.Sprintf(" (HTTP %d)", up.HTTPStatus)
		}
		return "upstream rejected the test request" + status + ": " + maskKeyPrefixes(up.Message)
	}
	return "request failed before the upstream answered: " + maskKeyPrefixes(err.Error())
}

// maskKeyPrefixes masks sk-/fc-/pk_-prefixed and Bearer-borne tokens — the
// credential shapes a leaked upstream message could carry.
func maskKeyPrefixes(msg string) string {
	for _, prefix := range []string{"sk-", "fc-", "pk_"} {
		if i := strings.Index(msg, prefix); i >= 0 {
			msg = msg[:i] + prefix + "[REDACTED]"
		}
	}
	if i := strings.Index(msg, "Bearer "); i >= 0 {
		msg = msg[:i] + "Bearer [REDACTED]"
	}
	return msg
}
