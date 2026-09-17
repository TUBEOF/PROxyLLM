package webui

// Provider-type validation (Task 5 Gap #2, plan §5.2 / Analysis G control 5):
// every provider PUT is validated against the canonical provider types
// before it reaches the storage layer. Invalid shapes are rejected with 400
// — the storage layer only checks JSON validity and secret-shaped keys, so
// a WebUI typo (wrong type string, missing base_url, wrong field name)
// would otherwise persist silently and only fail at first traffic.

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"

	"github.com/tubeof/proxyllm/internal/httpguard"
)

// Canonical provider types (plan §4.1, SPA PROVIDER_TYPES). The storage
// layer's seed strings are the same hyphenated forms (review fix `f9d6429`),
// so WebUI saves never rewrite a seeded provider's type.
const (
	ProviderTypeCustomOpenAI = "custom-openai"
	ProviderTypeOllamaCloud  = "ollama-cloud"
	ProviderTypeCodexOAuth   = "codex-oauth"
	ProviderTypeFirecrawl    = "firecrawl"
)

// OptOutEnv is the documented operator opt-out for the private-address
// SSRF guard (Analysis G control 5: base-URL validation with documented
// opt-out). Operators behind a proxy mesh that legitimately targets
// private backends set it to "1". Aliased from the shared guard package
// so the write path and the runtime redirect guard share one vocabulary.
const OptOutEnv = httpguard.OptOutEnv

// ProviderConfig is the validated shape of a provider config blob. Unknown
// keys are rejected (strict): a typo'd "baseurl" must fail the save, not
// silently route with the default URL. Exported so the server's
// construction path shares the exact same parse (one vocabulary).
type ProviderConfig struct {
	// custom-openai: upstream root including the version prefix (the
	// provider appends "/chat/completions"). Required.
	// ollama-cloud: overrides DefaultBaseURL ("https://ollama.com") when
	// set. Optional — an empty config is a valid save.
	// codex-oauth: unused (codex pins its backend URL).
	BaseURL string `json:"base_url"`
	// codex-oauth only: the OAuth issuer (e.g. https://auth.openai.com).
	// Required for codex-oauth (an issuer-less codex provider cannot ever
	// sign in).
	Issuer string `json:"issuer"`
	// Per-provider capability pins. Optional; only the seven known keys
	// are accepted.
	Capabilities map[string]bool `json:"capabilities"`
}

// knownCapabilities are the provider-level capability pins (snake_case,
// matching the IR capability vocabulary). Unknown keys are rejected.
var knownCapabilities = map[string]bool{
	"streaming":      true,
	"tools":          true,
	"vision":         true,
	"web_search":     true,
	"web_fetch":      true,
	"code_execution": true,
	"thinking":       true,
}

// MergeProviderConfig overlays own onto shared (ADR-009 decision 1: type
// settings first, the account's own fields win per key, shallow merge of
// the two JSON objects). A nil side contributes nothing. Exported because
// the server's construction path (server/providers.go) must merge with
// exactly the same rules the write path validated with — one vocabulary.
func MergeProviderConfig(shared, own json.RawMessage) (json.RawMessage, error) {
	var s, o map[string]json.RawMessage
	if len(shared) > 0 {
		if err := json.Unmarshal(shared, &s); err != nil {
			return nil, fmt.Errorf("stored type settings are not valid JSON")
		}
	}
	if len(own) > 0 {
		if err := json.Unmarshal(own, &o); err != nil {
			return nil, fmt.Errorf("account config is not valid JSON")
		}
	}
	if s == nil {
		s = map[string]json.RawMessage{}
	}
	for k, v := range o {
		s[k] = v
	}
	return json.Marshal(s)
}

// ParseProviderConfig validates (type, config) and returns the parsed
// shape. The error is operator-facing: it names the offending field, never
// the internals. An empty raw config (len 0) parses as an empty shape.
//
// This is the WRITE-path parse: base_url/issuer go through the private-
// address SSRF guard, which resolves DNS. Construction-time callers (the
// server building providers from stored rows) must use
// ParseProviderConfigStatic instead — a transient DNS failure at reload or
// startup must never drop a provider that passed the write-path guard.
func ParseProviderConfig(typ string, raw []byte) (ProviderConfig, error) {
	return parseProviderConfig(typ, raw, true)
}

// ParseProviderConfigStatic parses a stored provider config without the
// network-touching part of the guard (shape checks still apply: scheme,
// host, type fields, unknown keys). Rows reaching this path were already
// guarded when they were written.
func ParseProviderConfigStatic(typ string, raw []byte) (ProviderConfig, error) {
	return parseProviderConfig(typ, raw, false)
}

func parseProviderConfig(typ string, raw []byte, guardURLs bool) (ProviderConfig, error) {
	var cfg ProviderConfig
	if len(raw) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("config: %w", err)
		}
		// Strict input means the whole blob, not just its first value.
		if _, err := dec.Token(); err != io.EOF {
			return cfg, fmt.Errorf("config: unexpected trailing data")
		}
	}

	switch typ {
	case ProviderTypeCustomOpenAI:
		if cfg.BaseURL == "" {
			return cfg, fmt.Errorf("custom-openai requires base_url (the upstream OpenAI-compatible root, e.g. https://api.example.com/v1)")
		}
		if err := checkURL(cfg.BaseURL, "base_url", guardURLs); err != nil {
			return cfg, err
		}
		if cfg.Issuer != "" {
			return cfg, fmt.Errorf("base_url is the only URL field for custom-openai (issuer belongs to codex-oauth)")
		}
	case ProviderTypeOllamaCloud:
		if cfg.BaseURL != "" {
			if err := checkURL(cfg.BaseURL, "base_url", guardURLs); err != nil {
				return cfg, err
			}
		}
		if cfg.Issuer != "" {
			return cfg, fmt.Errorf("base_url is the only URL field for ollama-cloud (issuer belongs to codex-oauth)")
		}
	case ProviderTypeCodexOAuth:
		if cfg.Issuer == "" {
			return cfg, fmt.Errorf("codex-oauth requires issuer (the OAuth issuer, e.g. https://auth.openai.com)")
		}
		if err := checkURL(cfg.Issuer, "issuer", guardURLs); err != nil {
			return cfg, err
		}
		if cfg.BaseURL != "" {
			return cfg, fmt.Errorf("issuer is the only URL field for codex-oauth (the backend URL is pinned by the provider)")
		}
	case ProviderTypeFirecrawl:
		// base_url is optional: empty selects the firecrawl client's
		// documented default (https://api.firecrawl.dev) — self-hosted
		// instances and proxies override it.
		if cfg.BaseURL != "" {
			if err := checkURL(cfg.BaseURL, "base_url", guardURLs); err != nil {
				return cfg, err
			}
		}
		if cfg.Issuer != "" {
			return cfg, fmt.Errorf("base_url is the only URL field for firecrawl (issuer belongs to codex-oauth)")
		}
	case "":
		return cfg, fmt.Errorf("type is required (custom-openai, ollama-cloud, codex-oauth, firecrawl)")
	default:
		return cfg, fmt.Errorf("unknown provider type %q (custom-openai, ollama-cloud, codex-oauth, firecrawl)", typ)
	}

	for name := range cfg.Capabilities {
		if !knownCapabilities[name] {
			return cfg, fmt.Errorf("unknown capability %q (streaming, tools, vision, web_search, web_fetch, code_execution, thinking)", name)
		}
	}
	return cfg, nil
}

// checkURL validates an operator-supplied URL field. Shape checks (absolute
// http(s) URL with a host) always apply; the private-address check — which
// resolves DNS — runs only on the write path (guardURLs).
func checkURL(raw, field string, guardURLs bool) error {
	if err := urlShape(raw, field); err != nil {
		return err
	}
	if !guardURLs {
		return nil
	}
	return guardPublicURL(raw, field)
}

// urlShape validates an absolute http(s) URL with a host (no DNS).
func urlShape(raw, field string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	switch u.Scheme {
	case "http", "https":
	case "":
		return fmt.Errorf("%s: missing scheme (http or https)", field)
	default:
		return fmt.Errorf("%s: scheme %q not allowed (http or https)", field, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%s: missing host", field)
	}
	return nil
}

// guardPublicURL resolves the host of an operator-supplied URL and rejects
// private-address targets (Analysis G control 5 — the SSRF guard for
// WebUI-supplied provider endpoints). Write path only. Delegates the
// address classification to the shared guard package so the write path
// and the runtime redirect guard cannot drift apart.
func guardPublicURL(raw, field string) error {
	if httpguard.OptOut() {
		return nil
	}

	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	host := u.Hostname() // strip brackets for IPv6 literals
	addrs, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("%s: resolving %q: %w", field, host, err)
	}
	for _, addr := range addrs {
		if httpguard.IsPrivateAddr(addr) {
			return fmt.Errorf("%s: %q resolves to a private address (%s) — set %s=1 to allow private provider endpoints", field, host, addr, OptOutEnv)
		}
	}
	return nil
}
