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
	bearerRe = regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9\-._~+/=]{8,}`)
	// Credential-header values: x-api-key, x-auth-token, api-key,
	// authorization — including an optional scheme word (Token, Basic,
	// Digest, …; Bearer is handled by bearerRe). Anchored to the header
	// name so unprefixed prose survives.
	credHeaderRe = regexp.MustCompile(`(?i)\b(x-api-key|x-auth-token|api-key|authorization)\s*[:=]\s*['"]?(?:(?:token|basic|digest|apikey)\s+)?([A-Za-z0-9\-._~+/=]{16,})`)
	// AWS-style access key IDs (AKIA/ASIA + 16 uppercase alphanumerics).
	awsKeyRe = regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)
	// Credential values in explicit "secret (key)" context — e.g. AWS
	// secret access keys (40-char base64 with /+) reported by upstream
	// SDKs or config dumps. Anchored to the context word; the 16-char
	// floor keeps prose like "secret key rotation" untouched.
	secretKeyRe = regexp.MustCompile(`(?i)\bsecret(?:[_ ]?key)?\s*[:=]?\s*["']?([A-Za-z0-9/+._~=-]{16,})`)
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
	// JSON-form key/value pairs: "api_key": "value", "secret": "value", …
	// JSON key/value secrets: keep the key name, mask the value. The
	// alternation lists the multi-word names (secret_?key, refresh_?token,
	// access_?token) before their short prefixes: RE2 alternations match
	// leftmost-first, so a bare `secret` or `token` would otherwise claim
	// the key name and the extended spellings would never fire.
	jsonSecretRe = regexp.MustCompile(`(?i)["'](api_?key|access_?key|secret_?key|refresh_?token|access_?token|secret|token|password|credential)["']\s*:\s*["']([^"']{16,})["']`)
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

	// Credential headers: keep the header name, mask the value.
	value = credHeaderRe.ReplaceAllStringFunc(value, func(m string) string {
		idx := strings.IndexAny(m, ":=")
		if idx < 0 {
			return maskFor("header")
		}
		name := strings.TrimRight(m[:idx], " \t\"'")
		return name + "=" + maskFor("header")
	})

	value = awsKeyRe.ReplaceAllString(value, maskFor("aws"))
	// "secret key <value>" context: keep the context words, mask the value.
	value = secretKeyRe.ReplaceAllString(value, "secret key "+maskFor("secret"))
	value = skRe.ReplaceAllString(value, maskFor("sk"))
	value = fcRe.ReplaceAllString(value, maskFor("fc"))
	value = pkRe.ReplaceAllString(value, maskFor("pk"))
	value = jwtRe.ReplaceAllString(value, maskFor("jwt"))
	value = bigBlobRe.ReplaceAllString(value, maskFor("blob"))

	// JSON key/value secrets: keep the key name, mask the value.
	value = jsonSecretRe.ReplaceAllString(value, `$1`+": "+maskFor("secret"))

	// URL query secrets last so Bearer/key masking does not race with
	// query-param capture.
	value = urlTokenRe.ReplaceAllString(value, "$1="+maskFor("key"))

	return value
}
