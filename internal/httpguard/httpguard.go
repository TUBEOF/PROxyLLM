// Package httpguard centralizes the SSRF-relevant address classification
// and the guarded HTTP client construction.
//
// Two consumers share one vocabulary:
//
//   - the write path (internal/server/webui) validates operator-supplied
//     provider base URLs once, at config time — DNS-resolving the host and
//     rejecting private targets;
//   - the runtime dial path (provider transports) re-applies the same
//     guarantee on every HTTP redirect: Go's client follows up to 10
//     redirects and re-sends the Authorization header to the redirect
//     target, so an upstream that answers 302 toward the cloud metadata
//     endpoint would otherwise receive the operator's Bearer key.
//
// The package lives directly under internal/ (not inside a provider
// subtree) because both internal/server and internal/tools must import
// it — Go's internal-package rules would hide a deeper location from
// one of the two.
package httpguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

// OptOutEnv is the documented operator opt-out for the private-address
// SSRF guard. Operators behind a proxy mesh that
// legitimately targets private backends set it to "1".
const OptOutEnv = "PROXYLLM_ALLOW_PRIVATE_PROVIDER_URLS"

// GuardError marks a refusal issued by the redirect policy (not by the
// network). Callers use errors.As to distinguish "policy refused this
// redirect" from an ordinary transport failure.
type GuardError struct {
	// Location is the redirect target the policy rejected.
	Location string
	// Reason names the private-address class ("loopback", "RFC 1918", …).
	Reason string
}

func (e *GuardError) Error() string {
	return fmt.Sprintf("httpguard: refusing redirect to %s (%s)", e.Location, e.Reason)
}

// IsPrivateAddr reports whether addr is loopback, RFC 1918 private,
// link-local (incl. the cloud metadata endpoint), or otherwise a
// non-routable address class an SSRF should not reach. Shared by the
// write-path base-URL guard and the runtime redirect guard so the two
// never drift apart.
func IsPrivateAddr(addr net.IP) bool {
	return addr.IsLoopback() ||
		addr.IsPrivate() || // RFC 1918 (10/8, 172.16/12, 192.168/16)
		addr.IsLinkLocalUnicast() || // 169.254.0.0/16 incl. 169.254.169.254
		addr.IsLinkLocalMulticast() ||
		addr.IsUnspecified()
}

// OptOut reports whether the operator explicitly allowed private
// provider endpoints (documented opt-out).
func OptOut() bool {
	return os.Getenv(OptOutEnv) == "1"
}

// lookupIPAddr resolves a hostname to its addresses. Package-level so
// tests can stub resolution — CI must not depend on live DNS, and the
// happy path (public redirect target) is otherwise untestable against
// httptest's loopback-only listeners.
var lookupIPAddr = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return (&net.Resolver{}).LookupIPAddr(ctx, host)
}

// GuardLocation validates one redirect Location against the private-
// address policy: resolves the hostname and refuses when any resolved
// address is private.
func GuardLocation(ctx context.Context, loc *url.URL) error {
	if OptOut() {
		return nil
	}
	if loc == nil {
		return nil
	}
	host := loc.Hostname() // strip brackets and port; IPv6 literals too
	if host == "" {
		return &GuardError{Location: loc.String(), Reason: "missing host"}
	}
	addrs, err := lookupIPAddr(ctx, host)
	if err != nil {
		// An unresolvable redirect target is refused, not followed: the
		// write path (guardPublicURL) treats resolution failure the same
		// way, and following an unverifiable host would defeat the guard.
		return fmt.Errorf("httpguard: resolving redirect target %q: %w", host, err)
	}
	for _, addr := range addrs {
		if IsPrivateAddr(addr.IP) {
			return &GuardError{Location: loc.String(), Reason: privateReason(addr.IP)}
		}
	}
	return nil
}

// privateReason names the rejection class for operators.
func privateReason(addr net.IP) string {
	switch {
	case addr.IsLoopback():
		return "loopback"
	case addr.IsPrivate():
		return "RFC 1918"
	case addr.IsLinkLocalUnicast(), addr.IsLinkLocalMulticast():
		return "link-local"
	default:
		return "unspecified/non-routable"
	}
}

// defaultTimeout bounds a full non-streaming round trip (dial + headers +
// body). 30s matches the firecrawl emulation budget.
const defaultTimeout = 30 * time.Second

// newTransport is the tuned base for every guarded client.
//
// http.DefaultTransport's MaxIdleConnsPerHost is 2 — beyond two in-flight
// requests to one upstream, connections are not pooled at all, so a proxy
// running hundreds of concurrent requests to the same provider re-dials TLS for
// most of them. ResponseHeaderTimeout is the other half: without it an upstream
// that accepts a connection and then says nothing holds the goroutine (and,
// for a streaming client with no total timeout, holds it indefinitely).
//
// MaxConnsPerHost is deliberately left unset: a cap turns upstream slowness into
// queueing at our end, and the per-provider concurrency limits already bound the
// fan-out deliberately.
func newTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 200
	t.MaxIdleConnsPerHost = 32
	t.IdleConnTimeout = 90 * time.Second
	t.TLSHandshakeTimeout = 10 * time.Second
	t.ResponseHeaderTimeout = 30 * time.Second
	t.ExpectContinueTimeout = 1 * time.Second
	t.DialContext = (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	return t
}

// NewClient builds an http.Client whose redirect policy re-validates
// every redirect Location through the private-address guard. timeout is
// the TOTAL request budget (http.Client.Timeout covers dial + response
// headers + body read); pass 0 for the 30s default. Pass a negative
// timeout to build a streaming client (no total budget — long-lived
// streaming responses must not be cut off mid-body — while dial, TLS
// handshake, and response-header deadlines stay bounded through the
// tuned transport).
//
// Every client built here gets its OWN transport, so each caller has its own
// connection pool. Callers that should share a pool must share the client
// (see firecrawl's emulator client).
func NewClient(timeout time.Duration) *http.Client {
	transport := newTransport()
	if timeout < 0 {
		// Streaming: no total budget, but every phase still bounded.
		client := &http.Client{
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return GuardLocation(req.Context(), req.URL)
			},
		}
		return client
	}
	if timeout == 0 {
		timeout = defaultTimeout
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return GuardLocation(req.Context(), req.URL)
		},
	}
}

// NewStreamingClient builds an http.Client for long-lived streaming
// responses: no total body budget, but redirects are guarded and dial /
// TLS / response-header phases stay bounded through the shared default
// transport.
func NewStreamingClient() *http.Client {
	return NewClient(-1)
}
