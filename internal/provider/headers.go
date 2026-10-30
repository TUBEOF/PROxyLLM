package provider

import (
	"context"
	"net/http"
	"strings"
)

type forwardedHeadersCtxKey struct{}

// forwardable is the allow-list of client headers a same-family upstream may
// see. Credentials (Authorization, Cookie, Host) are never forwarded.
func forwardable(name string) bool {
	switch strings.ToLower(name) {
	case "anthropic-version", "anthropic-beta", "openai-beta":
		return true
	}
	return strings.HasPrefix(strings.ToLower(name), "x-stainless-")
}

// WithForwardedHeaders captures the allow-listed client headers in ctx. The
// API handlers call it once per request; provider HeaderHooks merge the
// result into their own header set.
func WithForwardedHeaders(ctx context.Context, h http.Header) context.Context {
	var out http.Header
	for name, vals := range h {
		if !forwardable(name) {
			continue
		}
		if out == nil {
			out = make(http.Header)
		}
		out[http.CanonicalHeaderKey(name)] = append([]string(nil), vals...)
	}
	if out == nil {
		return ctx
	}
	// A provider that sets its own header must win; MergeForwarded only
	// fills the gaps.
	return context.WithValue(ctx, forwardedHeadersCtxKey{}, out)
}

// ForwardedHeaders returns a copy of the captured headers (nil when none).
func ForwardedHeaders(ctx context.Context) http.Header {
	h, _ := ctx.Value(forwardedHeadersCtxKey{}).(http.Header)
	if h == nil {
		return nil
	}
	out := make(http.Header, len(h))
	for k, v := range h {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// MergeForwarded copies the captured headers into dst without overwriting a
// header the provider already set.
func MergeForwarded(ctx context.Context, dst http.Header) http.Header {
	if dst == nil {
		dst = make(http.Header)
	}
	for name, vals := range ForwardedHeaders(ctx) {
		if dst.Get(name) == "" {
			dst[name] = vals
		}
	}
	return dst
}
