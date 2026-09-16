package routing_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
)

// Error-classification contract tests, RED-GREEN. The table derives from
// the phase-1 research facts FO-1/FO-2 (phase-1-best-practices.md §3,
// accessed 2026-09-10) — never from implementation behavior:
//
//	FO-2: status codes alone are insufficient — the structured body
//	error.code is a tier ABOVE the HTTP-status tier:
//	  rate_limit_exceeded          → backoff + failover
//	  server_error                 → failover fast
//	  content_filter /
//	  content_policy_violation     → NEVER fail over (compliance risk)
//	  insufficient_quota           → fail over immediately
//	  400/401/403/404/422          → never retry
//	FO-1: early-stream failures (before first content byte: 408/429/5xx/TLS
//	reset) are safe to retry/fail over; mid-stream failures must NOT be
//	retried or provider-swapped.
//	FO-5: streaming requests are never retried once dispatched.

// upstreamErr builds a typed provider error the way providers return them
// from Complete (both classification tiers present).
func upstreamErr(status int, bodyCode, msg string) error {
	return &provider.UpstreamError{HTTPStatus: status, Code: bodyCode, Message: msg}
}

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		position routing.FailurePosition
		wantCls  routing.Class
		wantAct  routing.RetryAction
	}{
		// --- FO-1 transport tier: no HTTP response at all ---
		{"transport pre-stream fails over", errors.New("connection refused"), routing.PreStream,
			routing.ClassTransport, routing.ActionFailOver},
		{"TLS reset pre-stream fails over", errors.New("tls handshake reset"), routing.PreStream,
			routing.ClassTransport, routing.ActionFailOver},
		{"timeout pre-stream fails over", errors.New("i/o timeout"), routing.PreStream,
			routing.ClassTransport, routing.ActionFailOver},
		{"typed error with no status is transport", &provider.UpstreamError{Message: "no response"}, routing.PreStream,
			routing.ClassTransport, routing.ActionFailOver},

		// --- FO-2 tier 2: body error.code wins over the HTTP status ---
		{"429 + rate_limit_exceeded retries same provider with backoff", upstreamErr(429, "rate_limit_exceeded", "slow down"), routing.PreStream,
			routing.ClassRateLimit, routing.ActionRetrySame},
		{"529 + server_error fails over fast", upstreamErr(529, "server_error", "overloaded"), routing.PreStream,
			routing.ClassServerError, routing.ActionFailOver},
		{"content_filter never fails over", upstreamErr(400, "content_filter", "policy"), routing.PreStream,
			routing.ClassContentFilter, routing.ActionTerminal},
		{"content_policy_violation never fails over", upstreamErr(400, "content_policy_violation", "policy"), routing.PreStream,
			routing.ClassContentFilter, routing.ActionTerminal},
		{"insufficient_quota fails over immediately", upstreamErr(402, "insufficient_quota", "billing"), routing.PreStream,
			routing.ClassInsufficientQuota, routing.ActionFailOver},
		{"429 + insufficient_quota: body tier wins → failover, not retry", upstreamErr(429, "insufficient_quota", "billing"), routing.PreStream,
			routing.ClassInsufficientQuota, routing.ActionFailOver},

		// --- FO-2 tier 1: HTTP status when the body carries no known code ---
		{"429 without body code retries same", upstreamErr(429, "", "slow down"), routing.PreStream,
			routing.ClassRateLimit, routing.ActionRetrySame},
		{"408 fails over fast", upstreamErr(408, "", "timeout"), routing.PreStream,
			routing.ClassServerError, routing.ActionFailOver},
		{"500 fails over", upstreamErr(500, "", "boom"), routing.PreStream,
			routing.ClassServerError, routing.ActionFailOver},
		{"503 fails over", upstreamErr(503, "", "unavailable"), routing.PreStream,
			routing.ClassServerError, routing.ActionFailOver},
		{"529 (Anthropic overload) fails over", upstreamErr(529, "", "overloaded"), routing.PreStream,
			routing.ClassServerError, routing.ActionFailOver},
		{"400 never retries", upstreamErr(400, "", "bad request"), routing.PreStream,
			routing.ClassClientError, routing.ActionTerminal},
		{"401 never retries", upstreamErr(401, "", "unauthorized"), routing.PreStream,
			routing.ClassClientError, routing.ActionTerminal},
		{"403 never retries", upstreamErr(403, "", "forbidden"), routing.PreStream,
			routing.ClassClientError, routing.ActionTerminal},
		{"404 never retries", upstreamErr(404, "", "unknown model"), routing.PreStream,
			routing.ClassClientError, routing.ActionTerminal},
		{"422 never retries", upstreamErr(422, "", "invalid"), routing.PreStream,
			routing.ClassClientError, routing.ActionTerminal},

		// --- unknown body code falls through to the status tier ---
		{"unknown body code on 500 fails over via status tier", upstreamErr(500, "weird_new_code", "boom"), routing.PreStream,
			routing.ClassServerError, routing.ActionFailOver},
		{"unknown body code on 400 stays terminal via status tier", upstreamErr(400, "weird_new_code", "bad"), routing.PreStream,
			routing.ClassClientError, routing.ActionTerminal},

		// --- FO-1: mid-stream is ALWAYS terminal, regardless of class ---
		{"mid-stream 429 is terminal (FO-1 over FO-2)", upstreamErr(429, "rate_limit_exceeded", "slow down"), routing.MidStream,
			routing.ClassRateLimit, routing.ActionTerminal},
		{"mid-stream 500 is terminal", upstreamErr(500, "", "boom"), routing.MidStream,
			routing.ClassServerError, routing.ActionTerminal},
		{"mid-stream transport is terminal", errors.New("connection reset mid answer"), routing.MidStream,
			routing.ClassTransport, routing.ActionTerminal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := routing.ClassifyError(tc.err, tc.position)
			if got.Class != tc.wantCls || got.Action != tc.wantAct {
				t.Errorf("ClassifyError(%v, %s) = class %s action %s, want class %s action %s",
					tc.err, tc.position, got.Class, got.Action, tc.wantCls, tc.wantAct)
			}
			if got.FailurePosition != tc.position {
				t.Errorf("FailurePosition = %s, want %s (FO-4: trace must record it)",
					got.FailurePosition, tc.position)
			}
		})
	}
}

// Wrapped upstream errors must classify identically — providers wrap typed
// errors with request context (fmt.Errorf %w), and classification must not
// be defeated by wrapping.
func TestClassifyError_WrappedUpstreamError(t *testing.T) {
	inner := upstreamErr(429, "rate_limit_exceeded", "slow down")
	wrapped := fmt.Errorf("provider ollama: chat attempt: %w", inner)

	got := routing.ClassifyError(wrapped, routing.PreStream)
	if got.Class != routing.ClassRateLimit || got.Action != routing.ActionRetrySame {
		t.Errorf("wrapped classification = class %s action %s, want rate_limit/retry_same", got.Class, got.Action)
	}
}

// AsUpstreamError extracts both tiers for route_trace (FO-4: error_code +
// HTTP status recorded per attempt).
func TestAsUpstreamError(t *testing.T) {
	if _, _, ok := routing.AsUpstreamError(errors.New("plain")); ok {
		t.Error("plain error must not extract as upstream")
	}
	status, code, ok := routing.AsUpstreamError(upstreamErr(503, "server_error", "down"))
	if !ok || status != 503 || code != "server_error" {
		t.Errorf("AsUpstreamError = (%d, %q, %v), want (503, server_error, true)", status, code, ok)
	}
}
