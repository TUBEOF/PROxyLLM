// Package codex implements the ChatGPT/Codex OAuth provider: browser PKCE
// login, device-code login, lazily refreshed OAuth tokens, and the
// ChatGPT-backend Responses transport.
package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/tubeof/proxyllm/internal/httpguard"
	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/provider/internal/httpx"
	"github.com/tubeof/proxyllm/internal/translate"
	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/openai"
)

// Version rides the User-Agent: PROxyLLM pins its OWN version — the codex
// CLI itself pins a 0.0.0 build placeholder, so there is no stable CLI
// version string to mirror.
const Version = "0.1.0"

// DefaultClientVersion is the codex CLI release the model discovery
// announces. The ChatGPT backend gates the catalog on it: absent is a 400,
// and a stale or unknown value makes it answer with an empty list rather
// than an error — so the pin has to follow the CLI's own releases (npm
// @openai/codex; 0.163.0 at the fixture's access date) and is overridable
// per provider type (the client_version setting) for a deployment pinned to
// an older client.
const DefaultClientVersion = "0.163.0"

// Provider serves one Codex backend account over the Responses wire shape.
type Provider struct {
	id        string
	caps      provider.Capabilities
	auth      Auth
	client    *httpx.Client
	responses openai.ResponsesCodec

	// clientVersion rides the discovery request's client_version query.
	clientVersion string
}

// ProviderConfig wires the provider at construction time.
type ProviderConfig struct {
	ID           string
	BaseURL      string
	Capabilities provider.Capabilities
	Auth         Auth
	HTTP         *http.Client

	// ClientVersion overrides DefaultClientVersion; empty selects the pin.
	ClientVersion string

	// RateLimitSink, when set, receives the account's rate-limit snapshot
	// parsed off every response's x-codex-* headers — the backend has no
	// usage endpoint, so live traffic is the only honest source. Nil
	// disables the parse entirely.
	RateLimitSink func(RateLimits)
}

// New builds the provider. The transport talks {BaseURL}/codex/responses
// with the codex-rs header set and the Bearer sourced from the token store,
// lazily refreshed on expiry.
func New(cfg ProviderConfig) *Provider {
	httpc := cfg.HTTP
	if httpc == nil {
		// Streaming transport with guarded redirects — the OAuth access
		// token rides the Bearer header and would follow a redirect to a
		// private target on the default client.
		httpc = httpguard.NewStreamingClient()
	}
	client := &httpx.Client{
		BaseURL:    cfg.BaseURL,
		ChatPath:   "/codex/responses",
		ErrorShape: httpx.ErrorOpenAI,
		HTTP:       httpc,
	}
	clientVersion := cfg.ClientVersion
	if clientVersion == "" {
		clientVersion = DefaultClientVersion
	}
	p := &Provider{
		id:            cfg.ID,
		caps:          cfg.Capabilities,
		auth:          cfg.Auth,
		client:        client,
		responses:     openai.ResponsesCodec{},
		clientVersion: clientVersion,
	}
	if cfg.RateLimitSink != nil {
		// The sink runs on the dispatch path: parse here, hand the snapshot
		// over, and let the wiring layer decide what to persist — the
		// provider never touches storage itself.
		client.ResponseHeaderHook = func(h http.Header) {
			if rl, ok := rateLimitFromHeaders(h); ok {
				cfg.RateLimitSink(rl)
			}
		}
	}
	client.HeaderHook = p.headers
	return p
}

// ID identifies the provider instance (registry key).
func (p *Provider) ID() string { return p.id }

// Type reports the canonical provider type (ADR-009): this account is a
// codex-oauth account.
func (p *Provider) Type() string { return "codex-oauth" }

// HasCredential reports whether the token store holds an access token for
// this account (ADR-009 decision 4: a draft account whose device sign-in
// never completed stays out of type expansion).
func (p *Provider) HasCredential() bool {
	tok, err := p.auth.AccessToken(context.Background())
	return err == nil && tok.Access != ""
}

// Capabilities reports what the provider supports (routing filter input).
func (p *Provider) Capabilities() provider.Capabilities { return p.caps }

// headers supplies the per-request codex header set (including the Accept
// switch for streaming). The Bearer token comes from the auth store with
// lazy refresh BEFORE dispatch; the single-flight logic applies inside auth.
func (p *Provider) headers(ctx context.Context, stream bool) (http.Header, error) {
	tok, err := p.auth.AccessToken(ctx)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+tok.Access)
	h.Set("Originator", "codex_cli_rs")
	h.Set("User-Agent", "codex_cli_rs/proxyllm/"+Version)
	if tok.AccountID != "" {
		h.Set("Chatgpt-Account-Id", tok.AccountID)
	}
	// OAI-Product-Sku is deliberately NOT sent.
	if stream {
		h.Set("Accept", "text/event-stream")
	}
	// The platform tail "(os_type os_version; arch) terminal" is deliberately
	// omitted: it is telemetry, not auth material, and the backend is not
	// known to gate on it. If the live smoke run (PROXYLLM_SMOKE=1) shows
	// otherwise, the tail becomes a build/runtime stamp.
	return h, nil
}

// Complete performs one round trip. The ChatGPT backend has no
// non-streaming mode — a body without stream:true is answered 400
// {"detail":"Stream must be set to true"} (observed live 2026-10-17; the
// codex CLI always sends it, fixture openai-codex/responses-request.json) —
// so the round trip is dispatched streamed and assembled into the single
// response the non-streaming contract promises.
func (p *Provider) Complete(ctx context.Context, req ir.Request) (ir.Response, error) {
	events, cancel, err := p.Stream(ctx, req)
	if err != nil {
		return ir.Response{}, err
	}
	defer cancel()
	return assembleResponse(ctx, events)
}

// Stream performs one streaming round trip; the routing layer owns dispatch
// semantics, and the transport itself never retries.
func (p *Provider) Stream(ctx context.Context, req ir.Request) (<-chan ir.StreamEvent, func(), error) {
	return p.client.Stream(ctx, req, p.encode, func(r io.Reader) translate.StreamDecoder {
		return openai.NewResponsesStreamDecoder(r)
	})
}

// encode translates IR into the Responses wire request. stream is forced
// here: the backend rejects a non-streaming body, and this provider has no
// non-streaming dispatch (Complete assembles the stream instead).
func (p *Provider) encode(req ir.Request) ([]byte, error) {
	req.Stream = true
	return p.responses.EncodeRequest(req)
}

// RefreshCredentials rotates the OAuth pair on demand (routing health hook
// / operator action; single-flight + atomic persistence). Note: a rotation
// within the refresh cadence window (8 min) satisfies this as a no-op —
// a forced rotation (e.g. after a 401) must wait out that window; the lazy
// pre-expiry path in AccessToken bypasses it only after the interval, so a
// consumed-token scenario can never hide behind the no-op.
func (p *Provider) RefreshCredentials(ctx context.Context) error {
	return p.auth.Refresh(ctx)
}

// ListModels fetches the backend model catalog (ADR-010 model sync):
// GET {BaseURL}/codex/models?client_version=<codex-cli-semver> with the
// codex header set — the endpoint codex-rs itself refreshes its picker from
// (codex-rs model-provider/src/models_endpoint.rs and
// protocol/src/openai_models.rs, transcribed into
// testdata/golden/openai-codex/). The body is the manifest shape
// {"models":[{"slug",…}]}, NOT the OpenAI {"data":[{"id"}]} list: reading it
// as the latter is why this used to import nothing while the account was
// signed in.
//
// An empty catalog is an ERROR, not an empty model list: the backend
// filters by client_version and answers a stale one with no models, and
// "the upstream dropped every model" is exactly the reading that would let
// the sync delete the operator's whole codex row set. The syncer records
// the skip reason instead.
func (p *Provider) ListModels(ctx context.Context) ([]provider.ModelInfo, error) {
	raw, err := p.client.Get(ctx, "/codex/models?client_version="+url.QueryEscape(p.clientVersion))
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("codex: backend /codex/models body is not a model catalog: %w", err)
	}
	if len(manifest.Models) == 0 {
		return nil, fmt.Errorf("codex: backend returned an empty model catalog for client_version %s (a stale version is served as an empty list — is the pin current?)", p.clientVersion)
	}
	out := make([]provider.ModelInfo, 0, len(manifest.Models))
	seen := make(map[string]struct{}, len(manifest.Models))
	for _, m := range manifest.Models {
		if m.Slug == "" {
			continue
		}
		if _, dup := seen[m.Slug]; dup {
			continue
		}
		seen[m.Slug] = struct{}{}
		out = append(out, provider.ModelInfo{Name: m.Slug})
	}
	return out, nil
}

// PreviewRequest encodes the request exactly as Complete/Stream would send
// it (provider.RequestPreviewer): the Models page's test popup shows the
// operator what went out — for this backend the Responses wire body, always
// carrying stream:true.
func (p *Provider) PreviewRequest(req ir.Request) ([]byte, error) {
	return p.encode(req)
}
