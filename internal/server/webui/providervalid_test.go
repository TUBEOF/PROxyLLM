package webui

// Spec-derived tests for the provider config validator (Task 5 Gap #2,
// plan §5.2 / Analysis G control 5): shape checks, SSRF guard split
// (write path resolves DNS, construction path does not), strict parsing.

import "testing"

func TestParseProviderConfig_StaticSkipsDNS(t *testing.T) {
	// Same URL the write-path guard would DNS-resolve; without the opt-out
	// the static variant must still accept it because it never resolves.
	cfg, err := ParseProviderConfigStatic(ProviderTypeCustomOpenAI, []byte(`{"base_url":"https://api.invalid.example/v1"}`))
	if err != nil {
		t.Fatalf("static parse rejected a shape-valid URL: %v", err)
	}
	if cfg.BaseURL == "" {
		t.Fatal("base_url not parsed")
	}
}

func TestParseProviderConfig_StaticKeepsShapeChecks(t *testing.T) {
	for _, tc := range []struct {
		typ, raw string
	}{
		{ProviderTypeCustomOpenAI, `{}`},                                               // missing base_url
		{ProviderTypeCustomOpenAI, `{"base_url":"ftp://x/v1"}`},                        // bad scheme
		{ProviderTypeCustomOpenAI, `{"base_url":"https://x/v1","issuer":"https://i"}`}, // cross-type
		{ProviderTypeCodexOAuth, `{}`},                                                 // missing issuer
		{"custom", `{"base_url":"https://x/v1"}`},                                      // unknown type
		{ProviderTypeOllamaCloud, `{"unknown_key":1}`},                                 // unknown config key
	} {
		if _, err := ParseProviderConfigStatic(tc.typ, []byte(tc.raw)); err == nil {
			t.Errorf("static parse accepted %s %s", tc.typ, tc.raw)
		}
	}
}

func TestParseProviderConfig_TrailingDataRejected(t *testing.T) {
	if _, err := ParseProviderConfig(ProviderTypeOllamaCloud, []byte(`{"capabilities":{}} {"sneaky":true}`)); err == nil {
		t.Error("trailing JSON accepted")
	}
}

// Opt-out semantics (Analysis G control 5): with the opt-out the guard
// accepts a loopback URL; without it the write-path parse rejects it. The
// static variant is NOT affected by the opt-out because it never resolves.
func TestParseProviderConfig_OptOutOnlyAffectsWritePath(t *testing.T) {
	t.Setenv(OptOutEnv, "1")
	if _, err := ParseProviderConfig(ProviderTypeCustomOpenAI, []byte(`{"base_url":"http://127.0.0.1:8080/v1"}`)); err != nil {
		t.Fatalf("opted-out write-path parse rejected a loopback URL: %v", err)
	}
	t.Setenv(OptOutEnv, "")
	if _, err := ParseProviderConfig(ProviderTypeCustomOpenAI, []byte(`{"base_url":"http://127.0.0.1:8080/v1"}`)); err == nil {
		t.Error("write-path parse accepted a loopback URL without opt-out")
	}
	// Unchanged by the opt-out state:
	if _, err := ParseProviderConfigStatic(ProviderTypeCustomOpenAI, []byte(`{"base_url":"http://127.0.0.1:8080/v1"}`)); err != nil {
		t.Fatalf("static parse rejected a loopback URL: %v", err)
	}
}
