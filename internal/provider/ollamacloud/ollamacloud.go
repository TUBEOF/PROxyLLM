// Package ollamacloud implements the Ollama Cloud provider (plan §4.1):
// the hosted native API at https://ollama.com. It speaks the native
// /api/chat wire protocol (L1–L11) through the Ollama codec and the
// shared HTTP transport foundation — Ollama's OpenAI-compat surface is
// local-only, so cloud requests ride the native endpoint (L4).
package ollamacloud

import (
	"context"
	"io"

	"github.com/proxyllm/proxyllm/internal/httpguard"
	"github.com/proxyllm/proxyllm/internal/provider"
	"github.com/proxyllm/proxyllm/internal/provider/internal/httpx"
	"github.com/proxyllm/proxyllm/internal/translate"
	"github.com/proxyllm/proxyllm/internal/translate/ir"
	"github.com/proxyllm/proxyllm/internal/translate/ollama"
)

// DefaultBaseURL is the hosted Ollama Cloud API root (L1).
const DefaultBaseURL = "https://ollama.com"

// Provider is the Ollama Cloud provider: native /api/chat over HTTPS with
// Bearer auth (L2), NDJSON streaming, and the always-explicit stream field
// (L8).
type Provider struct {
	client  httpx.Client
	id      string
	caps    provider.Capabilities
	codec   ollama.Codec
	decoder func(r io.Reader) translate.StreamDecoder
}

// New constructs the Ollama Cloud provider. baseURL defaults to
// DefaultBaseURL when empty.
func New(id, baseURL, apiKey string, caps provider.Capabilities) *Provider {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Provider{
		client: httpx.Client{
			BaseURL:    baseURL,
			ChatPath:   "/api/chat", // native endpoint (L1/L4)
			APIKey:     apiKey,      // Bearer $OLLAMA_API_KEY (L2)
			ErrorShape: httpx.ErrorOllama,
			// Streaming transport: no total-body budget (long generations
			// must not be cut off), but redirects re-validated against the
			// private-address SSRF guard — the Bearer header would ride a
			// followed redirect.
			HTTP: httpguard.NewStreamingClient(),
		},
		id:    id,
		caps:  caps,
		codec: ollama.Codec{},
		decoder: func(r io.Reader) translate.StreamDecoder {
			return ollama.NewStreamDecoder(r)
		},
	}
}

// ID returns the stable, unique provider identifier.
func (p *Provider) ID() string { return p.id }

// Capabilities reports the configured capability set.
func (p *Provider) Capabilities() provider.Capabilities { return p.caps }

// Complete performs one non-streaming /api/chat round trip. The request
// body carries an explicit "stream":false (L8) — the codec emits it.
func (p *Provider) Complete(ctx context.Context, req ir.Request) (ir.Response, error) {
	return p.client.Complete(ctx, req, p.codec.EncodeRequest, p.codec.DecodeResponse)
}

// Stream starts one streaming /api/chat round trip (NDJSON lines).
func (p *Provider) Stream(ctx context.Context, req ir.Request) (<-chan ir.StreamEvent, func(), error) {
	return p.client.Stream(ctx, req, p.codec.EncodeRequest, p.decoder)
}

// RefreshCredentials is a no-op: Ollama Cloud authenticates with a static
// operator-supplied API key.
func (p *Provider) RefreshCredentials(_ context.Context) error { return nil }
