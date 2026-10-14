// Package routing implements the deterministic routing engine: rule model →
// chain resolution → health gate → failover loop, with the two-tier error
// classification and route_trace observability. Every transition (skip,
// retry, failover, compat-policy application) is recorded — fallbacks are
// never silent.
package routing

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"strings"

	"github.com/tubeof/proxyllm/internal/provider"
)

// Failure-position classification: a request
// that has not yet produced its first streamed content byte may be retried
// or failed over — the client has received nothing, so a retry is invisible
// to it. Once the first byte has reached the client, the failure is
// mid-stream and terminating with a protocol-conform error is the only
// correct response; swapping providers mid-stream corrupts the
// conversation the client already partially received.
type FailurePosition string

const (
	// PreStream means no content byte has been streamed to the client:
	// retry and failover are safe.
	PreStream FailurePosition = "pre_stream"

	// MidStream means content has already been streamed: never retried,
	// never swapped — the failure terminates the stream.
	MidStream FailurePosition = "mid_stream"
)

// RetryAction is the routing decision for one classified failure.
type RetryAction string

const (
	// ActionFailOver moves to the next chain entry.
	ActionFailOver RetryAction = "failover"

	// ActionRetrySame re-attempts the SAME provider after a backoff
	// (rate-limit class within its retry budget).
	ActionRetrySame RetryAction = "retry_same"

	// ActionTerminal ends the request: the error is not retryable and the
	// chain cannot help.
	ActionTerminal RetryAction = "terminal"
)

// Class is the two-tier error classification: tier 1 is the HTTP
// status, tier 2 the decoded body error.code — more specific than the
// status and therefore consulted first.
type Class string

const (
	// ClassRateLimit: 429 or body code rate_limit_exceeded — retry the
	// same provider with backoff (Retry-After honored), fail over once
	// the retry budget is spent.
	ClassRateLimit Class = "rate_limit"

	// ClassServerError: 5xx/529/408 or body code server_error — fail over
	// fast, no same-provider retry.
	ClassServerError Class = "server_error"

	// ClassContentFilter: body code content_filter /
	// content_policy_violation — NEVER retried and NEVER failed over:
	// the input is the problem; another provider would either reproduce
	// the refusal or answer around a policy the operator opted into.
	// That is a compliance risk, not a load problem.
	ClassContentFilter Class = "content_filter"

	// ClassInsufficientQuota: body code insufficient_quota — fail over
	// immediately, no same-provider retry (billing, not load).
	ClassInsufficientQuota Class = "insufficient_quota"

	// ClassClientError: HTTP 400/401/403/404/422 — never retried, never
	// failed over: the request (or its credentials) is invalid at any
	// provider.
	ClassClientError Class = "client_error"

	// ClassCapabilityGap: a 400 whose body code or message names an
	// unsupported input modality ("image", "multimodal",
	// "unsupported_content") — the request is valid, the model lacks a
	// capability. Fail over pre-stream; the executor additionally consults
	// the capability fallback models when the whole chain failed this way.
	ClassCapabilityGap Class = "capability_gap"

	// ClassContextWindow: a 400 whose body code or message says the input
	// exceeds the target model's context window — the request is valid, the
	// model is too small. Fail over pre-stream: another chain entry, or a
	// capability fallback model with a larger window, may serve it. This class
	// reaches the capability-fallback walk with a live request, so classifying
	// it terminal is exactly what made a vision fallback die on the first
	// (too small) fallback model instead of trying the next.
	ClassContextWindow Class = "context_window_exceeded"

	// ClassTransport: no HTTP response at all (connection refused, TLS
	// reset, timeout) — retryable/failover pre-stream.
	ClassTransport Class = "transport"

	// ClassNetwork: the request never reached the provider — the dial, the
	// name lookup or the TLS handshake failed before a byte was sent. The
	// provider may be perfectly healthy and only the path to it broken, so
	// this is never evidence about its health; it is a different class from a
	// connection that died mid-answer, which is. Fail over: the next entry
	// reaches a different host.
	ClassNetwork Class = "network"

	// ClassClientAbort: the answer is no longer wanted. Either the client
	// disconnected, or a budget of ours ran out. Terminal, because there is
	// nobody left to hand a failover to, and never evidence about the
	// provider — a cancelled request must not cool down a healthy account.
	ClassClientAbort Class = "client_abort"
)

// Classification is the outcome of classifying one failed attempt.
type Classification struct {
	// Class is the two-tier result.
	Class Class

	// Action is the retry-table decision for this failure.
	Action RetryAction

	// FailurePosition records pre-stream vs mid-stream for route_trace;
	// mid-stream is always terminal regardless of class.
	FailurePosition FailurePosition
}

// ClassifyError classifies err per the two-tier table. Position comes
// from the caller (the failover loop knows what has been streamed).
//
// Table:
//
//	Tier 2 first (body error.code — more specific than the status):
//	  rate_limit_exceeded          → rate_limit    → retry same (backoff), failover on budget exhaustion
//	  server_error                 → server_error  → failover fast
//	  content_filter /
//	  content_policy_violation     → content_filter → NEVER fail over
//	  insufficient_quota           → insufficient_quota → failover immediately
//	  context_length_exceeded      → context_window → failover (the model is too small, the request is fine)
//	Tier 1 (HTTP status, when no known body code):
//	  429                          → rate_limit    → retry same
//	  408, 5xx, 529                → server_error  → failover fast
//	  400/401/403/404/422          → client_error  → terminal
//	  no response (transport)      → transport     → failover pre-stream
//
// Mid-stream failures are ALWAYS terminal: the action applies
// only pre-stream. Enforced here, once, so the failover loop needs no
// position knowledge of its own.
func ClassifyError(err error, position FailurePosition) Classification {
	// A cancellation is not an observation about the service. Either the client
	// that asked for the answer is gone, or a budget of OURS ran out — and
	// neither says anything about the provider. It is terminal because there is
	// nobody left to hand a failover to, and it must not reach the health
	// tracker: one abandoned request would otherwise cool down a healthy
	// account for everyone.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return Classification{Class: ClassClientAbort, Action: ActionTerminal, FailurePosition: position}
	}
	c := classifyPreStream(err)
	// Once content has been streamed, no class justifies a retry or
	// a swap — the client holds a partial answer from this provider.
	if position == MidStream {
		return Classification{Class: c.Class, Action: ActionTerminal, FailurePosition: position}
	}
	c.FailurePosition = position
	return c
}

// classifyPreStream applies the body-then-status table to a failure,
// position aside.
func classifyPreStream(err error) Classification {
	// Before anything else: did the request reach the provider at all? A
	// failure that never got there says nothing about the service, and counting
	// it would let a broken network path mark healthy accounts as down.
	if neverReached(err) {
		return Classification{Class: ClassNetwork, Action: ActionFailOver}
	}
	// Tier 2: the structured body error code, when a typed upstream error
	// carries one. Status codes alone are insufficient — the body
	// code is the more specific signal.
	if status, code, ok := AsUpstreamError(err); ok {
		if status == 0 {
			// Typed error that never saw a response (the provider failed
			// before headers — e.g. wrapping a transport error): the
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
		// Capability-gap tier: a body code naming the modality, or a 400
		// message describing one. Only 400s qualify — a 404 mentioning
		// "image" is a routing problem, not a capability statement.
		if status == 400 && capabilityGapSignal(code, err) {
			return Classification{Class: ClassCapabilityGap, Action: ActionFailOver}
		}
		// Context-window tier: the same shape as the gap tier — a 400 the
		// request cannot fix, only a different target can.
		// Consulted AFTER the modality tier so a rejection that names both
		// (a vision-less model that also overflows) stays a capability gap:
		// the modality is the fact the operator configured a fallback for.
		if status == 400 && contextWindowSignal(code, err) {
			return Classification{Class: ClassContextWindow, Action: ActionFailOver}
		}
		// No known body code (or none at all): fall through to tier 1.
		return classifyByStatus(status)
	}
	// No typed upstream error: the connection opened and then failed, or a
	// deadline of ours expired while it was open. Either way the provider was
	// reachable, so this stays the class that counts against it.
	return Classification{Class: ClassTransport, Action: ActionFailOver}
}

// neverReached reports whether the failure happened before the request could be
// sent: a dial, a name lookup, a refused connection, a TLS handshake that did
// not complete. Go's transport wraps all of those in *net.OpError with Op
// "dial", including its own timeouts, so one check covers them.
//
// Deliberately narrow. A connection reset after the request went out looks
// similar from a distance and means something else entirely — the provider was
// there and then failed — so it is not included.
func neverReached(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Op == "dial"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	// A TLS handshake that fails for a certificate reason never sent the
	// request either: the connection was refused, not the request.
	var unknownAuth x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var certInvalid x509.CertificateInvalidError
	return errors.As(err, &unknownAuth) || errors.As(err, &hostnameErr) || errors.As(err, &certInvalid)
}

// classifyByStatus is tier 1 of the two-tier table: the HTTP status of the
// response.
func classifyByStatus(status int) Classification {
	switch {
	case status == 429:
		return Classification{Class: ClassRateLimit, Action: ActionRetrySame}
	case status == 408 || status >= 500:
		// 529 is the Anthropic overload status; >= 500 covers all
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
// code). Route_trace records both per attempt.
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

// contextWindowSignal reports whether the body code or the message text
// says the INPUT is larger than the target model's context window. Like the
// modality heuristic this is necessarily textual:
// only OpenAI and OpenRouter ship a stable body code
// ("context_length_exceeded"); Anthropic, Gemini, Mistral, llama.cpp and
// Bedrock state the overflow in prose only, in mutually different words.
// The substring list below is the collected set of vendor wordings, read
// on 2026-10-09, for the vendors PROxyLLM reaches, restricted to the phrasings
// that cannot be produced by an unrelated 400 (a bare "token" or "length"
// test would swallow validation errors such as "max_tokens must be greater
// than 0", which must stay terminal).
func contextWindowSignal(bodyCode string, err error) bool {
	if bodyCode == "context_length_exceeded" || bodyCode == "context_window_exceeded" {
		return true
	}
	var ue *provider.UpstreamError
	if !errors.As(err, &ue) {
		return false
	}
	msg := strings.ToLower(ue.Message)
	for _, marker := range []string{
		"maximum context length",                       // OpenAI, Azure, Mistral, OpenRouter
		"maximum context limit",                        // OpenAI siblings
		"exceeds the model's maximum context",          // Azure
		"longer than the model's context",              // provider-neutral wording
		"exceed context limit",                         // shared wording
		"exceeds the available context size",           // llama.cpp / Lemonade
		"input tokens exceed the configured limit",     // shared wording
		"exceeds the maximum number of tokens allowed", // Gemini
		"maximum input length",                         // Cerebras
		"prompt is too long",                           // Anthropic
		"prompt: length",                               // Anthropic siblings
		"input length exceeds the context length",      // llama.cpp
		"too many input tokens",                        // Bedrock
		"too many tokens",                              // Bedrock
		"input is too long",                            // Bedrock
	} {
		if strings.Contains(msg, marker) {
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
