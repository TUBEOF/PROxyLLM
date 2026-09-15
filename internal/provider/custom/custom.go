// Package custom implements the config-defined OpenAI-compatible provider
// (plan §4.1): arbitrary chat-completions endpoints attach by configuration
// alone. It speaks the OpenAI wire protocol (O1–O4) through the OpenAI
// codec and the shared HTTP transport foundation.
package custom

import (
	"context"
	"io"

	"github.com/proxyllm/proxyllm/internal/httpguard"
	"github.com/proxyllm/proxyllm/internal/provider"
	"github.com/proxyllm/proxyllm/internal/provider/internal/httpx"
	"github.com/proxyllm/proxyllm/internal/translate"
	"github.com/proxyllm/proxyllm/internal/translate/ir"
	"github.com/proxyllm/proxyllm/internal/translate/openai"
)

// Provider is one configured OpenAI-compatible endpoint. The provider ID,
// base URL, API key, and capability set all come from operator config.
type Provider struct {
	client  httpx.Client
	id      string
	caps    provider.Capabilities
	codec   openai.Codec
	decoder func(r io.Reader) translate.StreamDecoder
}

// New constructs a custom provider. baseURL is the API root including the
// version prefix (e.g. "https://api.example.com/v1"); chat completions
// POST to {baseURL}/chat/completions (O2).
func New(id, baseURL, apiKey string, caps provider.Capabilities) *Provider {
	return &Provider{
		client: httpx.Client{
			BaseURL:    baseURL,
			ChatPath:   "/chat/completions",
			APIKey:     apiKey,
			ErrorShape: httpx.ErrorOpenAI,
			// Streaming transport: no total-body budget (a legitimate long
			// generation must not be cut off), but every redirect is
			// re-validated against the private-address SSRF guard — Go
			// re-sends the Bearer header to redirect targets.
			HTTP: httpguard.NewStreamingClient(),
		},
		id:    id,
		caps:  caps,
		codec: openai.Codec{},
		decoder: func(r io.Reader) translate.StreamDecoder {
			return openai.NewStreamDecoder(r)
		},
	}
}

// ID returns the stable, unique provider identifier from config.
func (p *Provider) ID() string { return p.id }

// Capabilities reports the operator-configured capability set; routing
// filters and failsover on it.
func (p *Provider) Capabilities() provider.Capabilities { return p.caps }

// Complete performs one non-streaming chat-completions round trip.
func (p *Provider) Complete(ctx context.Context, req ir.Request) (ir.Response, error) {
	return p.client.Complete(ctx, req, p.codec.EncodeRequest, p.codec.DecodeResponse)
}

// Stream starts one streaming chat-completions round trip (data-only SSE,
// O4); canonical events arrive on the returned channel.
func (p *Provider) Stream(ctx context.Context, req ir.Request) (<-chan ir.StreamEvent, func(), error) {
	return p.client.Stream(ctx, req, p.codec.EncodeRequest, p.decoder)
}

// RefreshCredentials is a no-op: custom providers authenticate with a
// static operator-supplied key.
func (p *Provider) RefreshCredentials(_ context.Context) error { return nil }
