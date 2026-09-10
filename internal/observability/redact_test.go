package observability

import (
	"strings"
	"testing"
)

// Adversarial tests for the redaction layer. Spec: ADR-007 control 3 —
// the redactor masks Bearer tokens, sk-/fc-/pk- keys, JWTs, oversized
// base64 in any position: URLs, headers, JSON error bodies, free text.
// A secret that survives redaction is a defect, not a false positive.

func TestRedact_BearerToken(t *testing.T) {
	r := NewRedactor()
	in := "Authorization: Bearer sk-abc123DEF456ghi789"
	got := r.Redact(in)
	if strings.Contains(got, "sk-abc123DEF456ghi789") {
		t.Errorf("bearer secret survived: %q", got)
	}
	if !strings.Contains(got, "Bearer") {
		t.Errorf("structure lost: %q", got)
	}
}

func TestRedact_XApiKeyHeader(t *testing.T) {
	r := NewRedactor()
	in := "x-api-key: pk_live_9f8e7d6c5b4a3210"
	got := r.Redact(in)
	if strings.Contains(got, "pk_live_9f8e7d6c5b4a3210") {
		t.Errorf("x-api-key secret survived: %q", got)
	}
}

func TestRedact_SecretInURL(t *testing.T) {
	r := NewRedactor()
	tests := []struct {
		name, url string
	}{
		{"query param", "https://api.example.com/v1/search?api_key=sk-proj-abcdef123456"},
		{"token in path", "https://api.example.com/tokens/fc-0a1b2c3d4e5f6a7b8c"},
		{"api key query", "https://api.firecrawl.dev/v2?token=fc-XY12ab34cd56"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := r.Redact(tc.url)
			if strings.Contains(got, "sk-proj-abcdef123456") ||
				strings.Contains(got, "fc-0a1b2c3d4e5f6a7b8c") ||
				strings.Contains(got, "fc-XY12ab34cd56") {
				t.Errorf("secret survived in URL: %q", got)
			}
		})
	}
}

func TestRedact_JWT(t *testing.T) {
	r := NewRedactor()
	// Realistic JWT shape: three base64url segments, 20+ chars each.
	jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XL3MOnU0VvU1rxFQqZ8E"
	in := "error verifying token " + jwt + " for user"
	got := r.Redact(in)
	if strings.Contains(got, jwt) {
		t.Errorf("JWT survived: %q", got)
	}
}

func TestRedact_JSONErrorBody(t *testing.T) {
	r := NewRedactor()
	in := `{"error":{"message":"invalid key sk-supersecretvalue99 provided","type":"invalid_request_error"}}`
	got := r.Redact(in)
	if strings.Contains(got, "sk-supersecretvalue99") {
		t.Errorf("secret in JSON error body survived: %q", got)
	}
}

func TestRedact_OversizedBase64(t *testing.T) {
	r := NewRedactor()
	// A >256-char base64 blob is treated as secret material (could be a
	// key, cert, or image payload) and masked in logs.
	blob := strings.Repeat("QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVox", 10)
	got := r.Redact("payload: " + blob)
	if strings.Contains(got, blob) {
		t.Errorf("oversized base64 survived: %q", got)
	}
}

func TestRedact_PreservesBenignContent(t *testing.T) {
	r := NewRedactor()
	benign := []string{
		"model gpt-4o completed in 230ms with 1024 input tokens",
		"provider ollama-cloud healthy, latency 45ms",
		"route: claude-sonnet-5 -> codex (thinking=strip)",
		"user asked: what is the capital of France?",
	}
	for _, in := range benign {
		got := r.Redact(in)
		if got != in {
			t.Errorf("benign content mangled: in=%q got=%q", in, got)
		}
	}
}

func TestRedact_PkPrefixKey(t *testing.T) {
	r := NewRedactor()
	in := "client key pk_prox_A1b2C3d4E5f6G7h8J9k0L1 failed auth"
	got := r.Redact(in)
	if strings.Contains(got, "A1b2C3d4E5f6G7h8J9k0L1") {
		t.Errorf("pk_ key survived: %q", got)
	}
}

func TestRedact_EmptyAndEdge(t *testing.T) {
	r := NewRedactor()
	if got := r.Redact(""); got != "" {
		t.Errorf("empty input mangled: %q", got)
	}
	// Key too short to be a real secret must not be mangled — avoids
	// false positives like "fc-" in prose.
	if got := r.Redact("the model fc- is not available"); got != "the model fc- is not available" {
		t.Errorf("false positive on short fragment: %q", got)
	}
}

func TestRedact_MultipleSecretsOneLine(t *testing.T) {
	r := NewRedactor()
	in := "Bearer abcdef1234567890abcdef and key sk-9876543210zxywvu fed to fc-1122334455aa"
	got := r.Redact(in)
	for _, secret := range []string{"abcdef1234567890abcdef", "sk-9876543210zxywvu", "fc-1122334455aa"} {
		if strings.Contains(got, secret) {
			t.Errorf("secret survived: %q", got)
		}
	}
}
