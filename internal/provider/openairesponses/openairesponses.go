// Package openairesponses implements a generic OpenAI Responses API
// provider: POST {base}/v1/responses with Bearer authentication, SSE
// streaming, and the OpenAI Models API for discovery. It is the
// protocol-generic sibling of the Codex/ChatGPT provider.
package openairesponses

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
	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/openai"
)

// Provider is one configured OpenAI Responses endpoint.
type Provider struct {
	client httpx.Client
	id     string
	caps   provider.Capabilities
	codec  openai.ResponsesCodec
}

// New constructs the provider. baseURL is the API root (e.g.
// https://api.openai.com); the /v1/responses path rides the endpoint.
func New(id, baseURL, apiKey string, caps provider.Capabilities, opts ...httpguard.ClientOption) *Provider {
	return &Provider{
		client: httpx.Client{
			BaseURL:    strings.TrimRight(baseURL, "/"),
			ChatPath:   "/v1/responses",
			APIKey:     apiKey,
			ErrorShape: httpx.ErrorOpenAI,
			HTTP:       httpguard.NewStreamingClient(opts...),
			HeaderHook: func(ctx context.Context, _ bool) (http.Header, error) {
				return provider.MergeForwarded(ctx, make(http.Header)), nil
			},
		},
		id:    id,
		caps:  caps,
		codec: openai.ResponsesCodec{},
	}
}

// ID returns the stable provider identifier.
func (p *Provider) ID() string { return p.id }

// Type reports the canonical provider type.
func (p *Provider) Type() string { return "openai-responses" }

// Capabilities reports the operator-configured capability set.
func (p *Provider) Capabilities() provider.Capabilities { return p.caps }

// Complete performs one non-streaming Responses round trip.
func (p *Provider) Complete(ctx context.Context, req ir.Request) (ir.Response, error) {
	return p.client.Complete(ctx, req, p.codec.EncodeRequest, p.codec.DecodeResponse)
}

// Stream starts one streaming Responses round trip.
func (p *Provider) Stream(ctx context.Context, req ir.Request) (<-chan ir.StreamEvent, func(), error) {
	return p.client.Stream(ctx, req, p.codec.EncodeRequest, func(r io.Reader) translate.StreamDecoder {
		return openai.NewResponsesStreamDecoder(r)
	})
}

// RefreshCredentials is a no-op: static API key.
func (p *Provider) RefreshCredentials(_ context.Context) error { return nil }

// ListModels fetches the OpenAI model catalog.
func (p *Provider) ListModels(ctx context.Context) ([]provider.ModelInfo, error) {
	raw, err := p.client.Get(ctx, "/v1/models")
	if err != nil {
		return nil, err
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("openai-responses: decode /v1/models response: %w", err)
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

// PreviewRequest encodes the exact Responses body a call would send.
func (p *Provider) PreviewRequest(req ir.Request) ([]byte, error) {
	return p.codec.EncodeRequest(req)
}
