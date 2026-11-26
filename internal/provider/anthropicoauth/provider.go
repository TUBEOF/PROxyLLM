// Package anthropicoauth implements the Anthropic OAuth provider (ADR-022):
// the Messages wire through the shared anthropic codec, authenticated with a
// server-side OAuth bearer instead of x-api-key.
package anthropicoauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tubeof/proxyllm/internal/httpguard"
	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/provider/internal/httpx"
	"github.com/tubeof/proxyllm/internal/translate"
	"github.com/tubeof/proxyllm/internal/translate/anthropic"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// anthropicVersion is the Messages API version the provider announces.
const anthropicVersion = "2023-06-01"

// DefaultBaseURL is the Anthropic API root.
const DefaultBaseURL = "https://api.anthropic.com"

// Provider serves one Anthropic OAuth account over the Messages wire.
type Provider struct {
	client httpx.Client
	id     string
	caps   provider.Capabilities
	codec  anthropic.Codec
	auth   Auth
	// hasCredential is the routing-time answer, cached at construction.
	hasCredential bool
}

// New builds the provider. The bearer is read from the token store per
// dispatch and refreshed lazily; it is never a client-supplied credential.
func New(id, baseURL string, auth Auth, caps provider.Capabilities, opts ...httpguard.ClientOption) *Provider {
	base := strings.TrimRight(baseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	p := &Provider{id: id, caps: caps, codec: anthropic.Codec{}, auth: auth}
	p.client = httpx.Client{
		BaseURL:    base,
		ChatPath:   "/v1/messages",
		APIKey:     "", // the HeaderHook supplies the OAuth bearer
		ErrorShape: httpx.ErrorOpenAI,
		HTTP:       httpguard.NewStreamingClient(opts...),
		HeaderHook: func(ctx context.Context, stream bool) (http.Header, error) {
			tok, err := auth.AccessToken(ctx)
			if err != nil {
				return nil, err
			}
			h := provider.MergeForwarded(ctx, make(http.Header))
			if h.Get("anthropic-version") == "" {
				h.Set("anthropic-version", anthropicVersion)
			}
			// Never an x-api-key: this account type carries OAuth only.
			h.Del("x-api-key")
			h.Set("Authorization", "Bearer "+tok.Access)
			beta := h.Get("anthropic-beta")
			if beta == "" {
				beta = BetaHeader
			} else if !strings.Contains(beta, BetaHeader) {
				beta += "," + BetaHeader
			}
			h.Set("anthropic-beta", beta)
			if stream {
				h.Set("Accept", "text/event-stream")
			}
			return h, nil
		},
	}
	p.hasCredential = auth.HasToken(context.Background())
	return p
}

// ID returns the stable provider identifier.
func (p *Provider) ID() string { return p.id }

// Type reports the canonical provider type.
func (p *Provider) Type() string { return "anthropic-oauth" }

// Capabilities reports the operator's pins for this account.
func (p *Provider) Capabilities() provider.Capabilities { return p.caps }

// HasCredential reports whether a token exists (cached at construction).
func (p *Provider) HasCredential() bool { return p.hasCredential }

// Complete performs one non-streaming Messages round trip.
func (p *Provider) Complete(ctx context.Context, req ir.Request) (ir.Response, error) {
	return p.client.Complete(ctx, req, p.codec.EncodeRequest, p.codec.DecodeResponse)
}

// Stream starts one streaming Messages round trip.
func (p *Provider) Stream(ctx context.Context, req ir.Request) (<-chan ir.StreamEvent, func(), error) {
	return p.client.Stream(ctx, req, p.codec.EncodeRequest, func(r io.Reader) translate.StreamDecoder {
		return anthropic.NewStreamDecoder(r)
	})
}

// RefreshCredentials rotates the token pair (the health path calls it).
func (p *Provider) RefreshCredentials(ctx context.Context) error {
	return p.auth.Refresh(ctx)
}

// ListModels fetches the Anthropic Models API catalog.
func (p *Provider) ListModels(ctx context.Context) ([]provider.ModelInfo, error) {
	raw, err := p.client.Get(ctx, "/v1/models?limit=1000")
	if err != nil {
		return nil, err
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("anthropicoauth: decode /v1/models response: %w", err)
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
		out = append(out, provider.ModelInfo{Name: m.ID, Tools: true, Vision: true, Thinking: true})
	}
	return out, nil
}

// PreviewRequest encodes the exact Messages body a call would send.
func (p *Provider) PreviewRequest(req ir.Request) ([]byte, error) {
	return p.codec.EncodeRequest(req)
}
