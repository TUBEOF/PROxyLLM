// Package routing implements the deterministic routing engine (plan
// Analysis E): rule model → chain resolution → health gate → failover loop
// with the FO-1/FO-2 error classification and route_trace observability.
// Every transition (skip, retry, failover, compat-policy application) is
// recorded — fallbacks are never silent.
package routing

import (
	"errors"
	"strings"

	"github.com/tubeof/proxyllm/internal/provider"
)

// Failure-position classification (FO-1, phase-1 research §3): a request
// that has not yet produced its first streamed content byte may be retried
// or failed over — the client has received nothing, so a retry is invisible
// to it. Once the first byte has reached the client, the failure is
// mid-stream and terminating with a protocol-conform error is the only
// correct response; swapping providers mid-stream corrupts the
// conversation the client already partially received.
type FailurePosition string

const (
	// PreStream means no content byte has been streamed to the client:
	// retry and failover are safe (FO-1).
	PreStream FailurePosition = "pre_stream"

	// MidStream means content has already been streamed: never retried,
	// never swapped — the failure terminates the stream (FO-1/FO-5).
	MidStream FailurePosition = "mid_stream"
)

// RetryAction is the routing decision for one classified failure.
type RetryAction string

const (
	// ActionFailOver moves to the next chain entry.
	ActionFailOver RetryAction = "failover"

	// ActionRetrySame re-attempts the SAME provider after a backoff
	// (rate-limit class within its retry budget, FO-2/FO-3).
	ActionRetrySame RetryAction = "retry_same"

	// ActionTerminal ends the request: the error is not retryable and the
	// chain cannot help.
	ActionTerminal RetryAction = "terminal"
)

// Class is the two-tier error classification (FO-2): tier 1 is the HTTP
// status, tier 2 the decoded body error.code — more specific than the
// status and therefore consulted first.
type Class string

const (
	// ClassRateLimit: 429 or body code rate_limit_exceeded — retry the
	// same provider with backoff (Retry-After honored), fail over once
	// the retry budget is spent (FO-2/FO-3).
	ClassRateLimit Class = "rate_limit"

	// ClassServerError: 5xx/529/408 or body code server_error — fail over
	// fast, no same-provider retry (FO-2).
	ClassServerError Class = "server_error"

	// ClassContentFilter: body code content_filter /
	// content_policy_violation — NEVER retried and NEVER failed over:
	// the input is the problem; another provider would either reproduce
	// the refusal or answer around a policy the operator opted into
	// (FO-2: compliance risk).
	ClassContentFilter Class = "content_filter"

	// ClassInsufficientQuota: body code insufficient_quota — fail over
	// immediately, no same-provider retry (billing, not load; FO-2).
	ClassInsufficientQuota Class = "insufficient_quota"

	// ClassClientError: HTTP 400/401/403/404/422 — never retried, never
	// failed over: the request (or its credentials) is invalid at any
	// provider (FO-2).
	ClassClientError Class = "client_error"

	// ClassCapabilityGap: a 400 whose body code or message names an
	// unsupported input modality ("image", "multimodal",
	// "unsupported_content") — the request is valid, the model lacks a
	// capability. Fail over pre-stream (2026-09-21 automatic capabilities);
	// the executor additionally consults the capability fallback models
	// when the whole chain failed this way.
	ClassCapabilityGap Class = "capability_gap"

	// ClassTransport: no HTTP response at all (connection refused, TLS
	// reset, timeout) — retryable/failover pre-stream (FO-1).
	ClassTransport Class = "transport"
)

// Classification is the outcome of classifying one failed attempt.
type Classification struct {
	// Class is the FO-2 two-tier result.
	Class Class

	// Action is the retry-table decision for this failure.
	Action RetryAction

	// FailurePosition records pre-stream vs mid-stream for route_trace
	// (FO-4); mid-stream is always terminal regardless of class.
	FailurePosition FailurePosition
}

// ClassifyError classifies err per the FO-2 two-tier table. Position comes
// from the caller (the failover loop knows what has been streamed).
//
// Table (FO-2, phase-1 research §3, accessed 2026-09-10):
//
//	Tier 2 first (body error.code — more specific than the status):
//	  rate_limit_exceeded          → rate_limit    → retry same (backoff), failover on budget exhaustion
//	  server_error                 → server_error  → failover fast
//	  content_filter /
//	  content_policy_violation     → content_filter → NEVER fail over
//	  insufficient_quota           → insufficient_quota → failover immediately
//	Tier 1 (HTTP status, when no known body code):
//	  429                          → rate_limit    → retry same
//	  408, 5xx, 529                → server_error  → failover fast
//	  400/401/403/404/422          → client_error  → terminal
//	  no response (transport)      → transport     → failover pre-stream
//
// Mid-stream failures are ALWAYS terminal (FO-1/FO-5): the action applies
// only pre-stream. Enforced here, once, so the failover loop needs no
// position knowledge of its own.
func ClassifyError(err error, position FailurePosition) Classification {
	c := classifyPreStream(err)
	// FO-1: once content has been streamed, no class justifies a retry or
	// a swap — the client holds a partial answer from this provider.
	if position == MidStream {
		return Classification{Class: c.Class, Action: ActionTerminal, FailurePosition: position}
	}
	c.FailurePosition = position
	return c
}

// classifyPreStream applies the FO-2 table to a failure, position aside.
func classifyPreStream(err error) Classification {
	// Tier 2: the structured body error code, when a typed upstream error
	// carries one. Status codes alone are insufficient (FO-2) — the body
	// code is the more specific signal.
	if status, code, ok := AsUpstreamError(err); ok {
		if status == 0 {
			// Typed error that never saw a response (the provider failed
			// before headers — e.g. wrapping a transport error): FO-1's
			// retryable class, not a server-error failover.
			return Classification{Class: ClassTransport, Action: ActionFailOver}
		}
		switch code {
		case "rate_limit_exceeded":
			return Classification{Class: ClassRateLimit, Action: ActionRetrySame}
		case "server_error":
			return Classification{Class: ClassServerError, Action: ActionFailOver}
		case "content_filter", "content_policy_violation":
			return Classification{Class: ClassContentFilter, Action: ActionTerminal}
		case "insufficient_quota":
			return Classification{Class: ClassInsufficientQuota, Action: ActionFailOver}
		}
		// Capability-gap tier (2026-09-21 automatic capabilities): a body
		// code naming the modality or a 400 message describing one. Only
		// 400s qualify — a 404 mentioning "image" is a routing problem,
		// not a capability statement.
		if status == 400 && capabilityGapSignal(code, err) {
			return Classification{Class: ClassCapabilityGap, Action: ActionFailOver}
		}
		// No known body code (or none at all): fall through to tier 1.
		return classifyByStatus(status)
	}
	// No typed upstream error: the request never produced a response —
	// connection refused, TLS reset, timeout. FO-1 names exactly these as
	// the retryable/failover class.
	return Classification{Class: ClassTransport, Action: ActionFailOver}
}

// classifyByStatus is FO-2 tier 1: the HTTP status of the response.
func classifyByStatus(status int) Classification {
	switch {
	case status == 429:
		return Classification{Class: ClassRateLimit, Action: ActionRetrySame}
	case status == 408 || status >= 500:
		// 529 is the Anthropic overload status (A13); >= 500 covers all
		// server statuses including it.
		return Classification{Class: ClassServerError, Action: ActionFailOver}
	case status == 400 || status == 401 || status == 403 ||
		status == 404 || status == 422:
		return Classification{Class: ClassClientError, Action: ActionTerminal}
	default:
		// A typed upstream error with an unclassifiable status and no
		// known body code (e.g. 402 from a provider without the quota
		// code, or 3xx oddities): fail over pre-stream rather than
		// killing the request on one provider's odd status — the chain
		// may route around it; the retry budget bounds the cost.
		return Classification{Class: ClassServerError, Action: ActionFailOver}
	}
}

// AsUpstreamError reports whether err (or anything it wraps) is a typed
// upstream error carrying the two classification tiers (status + body
// code). Route_trace records both per attempt (FO-4).
func AsUpstreamError(err error) (status int, bodyCode string, ok bool) {
	var ue *provider.UpstreamError
	if errors.As(err, &ue) {
		return ue.HTTPStatus, ue.Code, true
	}
	return 0, "", false
}

// capabilityGapSignal reports whether the body code or the message text
// names an unsupported input modality — the heuristic tier of the
// capability-gap class. Vendors word these rejections differently, so both
// sources are consulted, case-insensitively. Known body codes:
// unsupported_content / image_not_supported. Message markers: "image" or
// "images" (word-bounded), "multimodal", "unsupported content type".
func capabilityGapSignal(bodyCode string, err error) bool {
	switch bodyCode {
	case "unsupported_content", "image_not_supported":
		return true
	}
	var ue *provider.UpstreamError
	if !errors.As(err, &ue) {
		return false
	}
	msg := strings.ToLower(ue.Message)
	if strings.Contains(msg, "unsupported content type") {
		return true
	}
	// "image(s)" as a word: bounded start, optional plural, bounded end
	// ("image input", "images are not supported"; not "imagesque").
	for _, stem := range []string{"image", "images", "multimodal"} {
		if wordBounded(msg, stem) {
			return true
		}
	}
	return false
}

// wordBounded reports whether needle appears in hay as a whole word
// ("image" matches "image input", not "imagesque").
func wordBounded(hay, needle string) bool {
	for i := 0; ; i++ {
		idx := strings.Index(hay[i:], needle)
		if idx < 0 {
			return false
		}
		start := i + idx
		end := start + len(needle)
		if start > 0 && isWordRune(hay[start-1]) {
			i = start + 1
			continue
		}
		if end < len(hay) && isWordRune(hay[end]) {
			i = start + 1
			continue
		}
		return true
	}
}

// isWordRune reports whether r continues a word (letters/digits/underscore)
// — the boundary complement for wordBounded.
func isWordRune(r byte) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}
