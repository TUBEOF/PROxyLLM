package observability

import (
	"regexp"
	"strings"
)

// Redactor is the single gate through which all potentially sensitive
// values pass before they reach a log line or error message.
type Redactor interface {
	// Redact returns value with all recognized secret patterns masked.
	Redact(value string) string
}

// redactor implements Redact with pattern-based masking. Patterns are
// anchored to plausible secret shapes so benign content (model names,
// prose) survives untouched.
type redactor struct{}

// Compiled patterns. Each replaces the whole match with a mask that
// preserves the recognized prefix so operators can still tell what kind of
// secret was present.
var (
	// Bearer tokens: "Bearer <token>" — mask the token, keep "Bearer".
	bearerRe = regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9\-._~+/]{8,}`)
	// sk- keys (OpenAI-style): length floor avoids prose false positives.
	skRe = regexp.MustCompile(`\bsk-[A-Za-z0-9\-_]{12,}\b`)
	// fc- keys (Firecrawl-style).
	fcRe = regexp.MustCompile(`\bfc-[A-Za-z0-9\-_]{12,}\b`)
	// pk_ keys (PROxyLLM client keys).
	pkRe = regexp.MustCompile(`\bpk_[A-Za-z0-9\-_]{4,}\b`)
	// JWTs: three base64url segments, each length >= 20.
	jwtRe = regexp.MustCompile(`\beyJ[A-Za-z0-9\-_]{18,}\.[A-Za-z0-9\-_]{18,}\.[A-Za-z0-9\-_]{18,}\b`)
	// Generic key/token query params in URLs.
	urlTokenRe = regexp.MustCompile(`(?i)(api_key|token|key|secret)=([A-Za-z0-9\-._~+/]{8,})`)
	// Oversized base64-ish blobs (>= 256 chars) — likely key/cert/image
	// material that should not appear in logs verbatim.
	bigBlobRe = regexp.MustCompile(`[A-Za-z0-9+/]{256,}={0,2}`)
)

// NewRedactor returns the default Redactor.
func NewRedactor() Redactor {
	return &redactor{}
}

// maskFor returns the mask placeholder for a pattern family; the kind
// marker lets operators see what was redacted without seeing the secret.
func maskFor(kind string) string { return "[REDACTED:" + kind + "]" }

// Redact masks every recognized secret pattern in value.
func (*redactor) Redact(value string) string {
	if value == "" {
		return value
	}

	// Bearer: keep the scheme word, mask the credential.
	value = bearerRe.ReplaceAllStringFunc(value, func(m string) string {
		// m is "Bearer <token>"; split on the first whitespace run.
		scheme, _, _ := strings.Cut(m, " ")
		return scheme + " " + maskFor("bearer")
	})

	value = skRe.ReplaceAllString(value, maskFor("sk"))
	value = fcRe.ReplaceAllString(value, maskFor("fc"))
	value = pkRe.ReplaceAllString(value, maskFor("pk"))
	value = jwtRe.ReplaceAllString(value, maskFor("jwt"))
	value = bigBlobRe.ReplaceAllString(value, maskFor("blob"))

	// URL query secrets last so Bearer/key masking does not race with
	// query-param capture.
	value = urlTokenRe.ReplaceAllString(value, "$1="+maskFor("key"))

	return value
}
