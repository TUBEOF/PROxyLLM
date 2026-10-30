// Package anthropic implements the native Anthropic Messages provider:
// POST {base}/v1/messages with x-api-key authentication, SSE streaming, and
// the Anthropic Models API for discovery. It reuses the anthropic codec pair
// and the shared guarded HTTP transport.
package anthropic

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

// anthropicVersion is the Messages API version the provider announces. It is
// a wire fact; bump it with the fixture when the reference changes.
const anthropicVersion = "2023-06-01"

// Provider is one configured Anthropic Messages account.
type Provider struct {
	client httpx.Client
	id     string
	caps   provider.Capabilities
	codec  anthropic.Codec
}

// New constructs the provider. baseURL is the API root (e.g.
// https://api.anthropic.com); the Messages path rides the endpoint.
func New(id, baseURL, apiKey string, caps provider.Capabilities, opts ...httpguard.ClientOption) *Provider {
	base := strings.TrimRight(baseURL, "/")
	return &Provider{
		client: httpx.Client{
			BaseURL:  base,
			ChatPath: "/v1/messages",
			// x-api-key is the Anthropic auth header; the shared transport's
			// Bearer default is deliberately left empty.
			APIKey:     "",
			ErrorShape: httpx.ErrorOpenAI,
			HTTP:       httpguard.NewStreamingClient(opts...),
			HeaderHook: func(ctx context.Context, stream bool) (http.Header, error) {
				h := provider.MergeForwarded(ctx, make(http.Header))
				if h.Get("anthropic-version") == "" {
					h.Set("anthropic-version", anthropicVersion)
				}
				if apiKey != "" && h.Get("x-api-key") == "" {
					h.Set("x-api-key", apiKey)
				}
				if stream {
					h.Set("Accept", "text/event-stream")
				}
				return h, nil
			},
		},
		id:    id,
		caps:  caps,
		codec: anthropic.Codec{},
	}
}

// ID returns the stable provider identifier.
func (p *Provider) ID() string { return p.id }

// Type reports the canonical provider type.
func (p *Provider) Type() string { return "anthropic" }

// Capabilities reports what the operator configured for this account.
func (p *Provider) Capabilities() provider.Capabilities { return p.caps }

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

// RefreshCredentials is a no-op: the provider uses a static API key.
func (p *Provider) RefreshCredentials(_ context.Context) error { return nil }

// ListModels fetches the Anthropic Models API catalog. Claude models are
// homogeneous for tools/vision/thinking; the operator can still pin a model
// row. Context length is not reported by the catalog.
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
		return nil, fmt.Errorf("anthropic: decode /v1/models response: %w", err)
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
		out = append(out, provider.ModelInfo{
			Name: m.ID, Tools: true, Vision: true, Thinking: true,
		})
	}
	return out, nil
}

// PreviewRequest encodes the exact Messages body a call would send.
func (p *Provider) PreviewRequest(req ir.Request) ([]byte, error) {
	return p.codec.EncodeRequest(req)
}
