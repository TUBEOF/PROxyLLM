// Package ollamacloud implements the Ollama Cloud provider (plan §4.1):
// the hosted OpenAI-compatible API at https://ollama.com/v1. It speaks the
// OpenAI chat-completions wire (O1–O4) through the OpenAI codec and the
// shared HTTP transport foundation. The OpenAI-compat surface is the
// cloud API's primary chat transport (docs.ollama.com/api/
// openai-compatibility — cloud access is base_url https://ollama.com/v1
// with an OLLAMA_API_KEY bearer); the native /api/chat wire fragmented
// interleaved thinking traces into separate content blocks (the
// 2026-09-22 Claude Code rendering bug), which the OpenAI surface avoids.
// Local Ollama servers attach via the custom-openai provider type.
package ollamacloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/tubeof/proxyllm/internal/httpguard"
	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/provider/internal/httpx"
	"github.com/tubeof/proxyllm/internal/translate"
	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/openai"
)

// DefaultBaseURL is the hosted Ollama Cloud API root (L1). The /v1
// version prefix rides the endpoint paths (custom-openai convention).
const DefaultBaseURL = "https://ollama.com"

// Provider is the Ollama Cloud provider: the OpenAI-compatible
// /v1/chat/completions over HTTPS with Bearer auth (L2), data-only SSE
// streaming (O4).
type Provider struct {
	client  httpx.Client
	id      string
	caps    provider.Capabilities
	codec   openai.Codec
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
			ChatPath:   "/v1/chat/completions", // OpenAI-compat endpoint
			APIKey:     apiKey,                 // Bearer $OLLAMA_API_KEY (L2)
			ErrorShape: httpx.ErrorOpenAI,
			// Streaming transport: no total-body budget (long generations
			// must not be cut off), but redirects re-validated against the
			// private-address SSRF guard — the Bearer header would ride a
			// followed redirect.
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

// ID returns the stable, unique provider identifier.
func (p *Provider) ID() string { return p.id }

// Type reports the canonical provider type (ADR-009): this account is an
// ollama-cloud account. Deliberately NO HasCredential (same reasoning as
// custom: the construction contract treats a missing key as legal).
func (p *Provider) Type() string { return "ollama-cloud" }

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

// ListModels fetches the upstream model list (ADR-010 model sync) —
// GET /v1/models, the OpenAI list shape. The entry "id" field is the wire
// model identifier (the /v1/chat/completions "model" value). NOTE: cloud
// IDs may differ from the native /api/tags "name" values — model rows and
// routing rules reference these IDs.
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
		return nil, fmt.Errorf("ollamacloud: decode /v1/models response: %w", err)
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

// RefreshCredentials is a no-op: Ollama Cloud authenticates with a static
// operator-supplied API key.
func (p *Provider) RefreshCredentials(_ context.Context) error { return nil }
