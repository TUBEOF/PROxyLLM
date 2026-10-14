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

// Provider serves one Codex backend account over the Responses wire shape.
type Provider struct {
	id        string
	caps      provider.Capabilities
	auth      Auth
	client    *httpx.Client
	responses openai.ResponsesCodec
}

// ProviderConfig wires the provider at construction time.
type ProviderConfig struct {
	ID           string
	BaseURL      string
	Capabilities provider.Capabilities
	Auth         Auth
	HTTP         *http.Client
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
	p := &Provider{
		id:        cfg.ID,
		caps:      cfg.Capabilities,
		auth:      cfg.Auth,
		client:    client,
		responses: openai.ResponsesCodec{},
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

// Complete performs one non-streaming round trip.
func (p *Provider) Complete(ctx context.Context, req ir.Request) (ir.Response, error) {
	return p.client.Complete(ctx, req, p.encode, decodeResponse)
}

// Stream performs one streaming round trip; the routing layer owns dispatch
// semantics, and the transport itself never retries.
func (p *Provider) Stream(ctx context.Context, req ir.Request) (<-chan ir.StreamEvent, func(), error) {
	return p.client.Stream(ctx, req, p.encode, func(r io.Reader) translate.StreamDecoder {
		return openai.NewResponsesStreamDecoder(r)
	})
}

// encode translates IR into the Responses wire request.
func (p *Provider) encode(req ir.Request) ([]byte, error) {
	return p.responses.EncodeRequest(req)
}

// decodeResponse translates a Responses wire body back into IR.
func decodeResponse(raw []byte) (ir.Response, error) {
	var codec openai.ResponsesCodec
	return codec.DecodeResponse(raw)
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

// ListModels fetches the backend model list (ADR-010 model sync) —
// GET {BaseURL}/models with the codex header set. There is no pinned wire
// contract for this backend endpoint (unlike the transport endpoints above):
// the lister accepts the OpenAI list shape {data:[{id}]} when the backend
// serves it and surfaces every other outcome (404, auth failure, unexpected
// body) as a typed error — the syncer skips the account and records the
// reason; it never fails the whole run.
func (p *Provider) ListModels(ctx context.Context) ([]provider.ModelInfo, error) {
	raw, err := p.client.Get(ctx, "/models")
	if err != nil {
		return nil, err
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("codex: backend /models body is not a model list: %w", err)
	}
	out := make([]provider.ModelInfo, 0, len(list.Data))
	seen := make(map[string]struct{}, len(list.Data))
	for _, m := range list.Data {
		if m.ID == "" {
			continue
		}
		if _, dup := seen[m.ID]; dup {
			continue
		}
		seen[m.ID] = struct{}{}
		out = append(out, provider.ModelInfo{Name: m.ID})
	}
	return out, nil
}
