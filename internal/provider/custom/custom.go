// Package custom implements the config-defined OpenAI-compatible provider:
// arbitrary chat-completions endpoints attach by configuration
// alone. It speaks the OpenAI wire protocol through the OpenAI
// codec and the shared HTTP transport foundation.
package custom

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
// POST to {baseURL}/chat/completions.
// opts tune the guarded transport (e.g. ADR-018's
// response-header wall); none keeps the shared defaults.
func New(id, baseURL, apiKey string, caps provider.Capabilities, opts ...httpguard.ClientOption) *Provider {
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
			HTTP: httpguard.NewStreamingClient(opts...),
			HeaderHook: func(ctx context.Context, _ bool) (http.Header, error) {
				return provider.MergeForwarded(ctx, make(http.Header)), nil
			},
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

// Type reports the canonical provider type (ADR-009): this account is a
// custom-openai endpoint. A chain entry naming the type expands to all
// accounts of it. Deliberately NO HasCredential: a keyless custom endpoint
// is a legal routing state (the construction path routes without a key;
// local endpoints are auth-free), so every constructed account joins the
// type expansion.
func (p *Provider) Type() string { return "custom-openai" }

// Capabilities reports the operator-configured capability set; routing
// filters and failsover on it.
func (p *Provider) Capabilities() provider.Capabilities { return p.caps }

// Complete performs one non-streaming chat-completions round trip.
func (p *Provider) Complete(ctx context.Context, req ir.Request) (ir.Response, error) {
	return p.client.Complete(ctx, req, p.codec.EncodeRequest, p.codec.DecodeResponse)
}

// Stream starts one streaming chat-completions round trip (data-only SSE);
// canonical events arrive on the returned channel.
func (p *Provider) Stream(ctx context.Context, req ir.Request) (<-chan ir.StreamEvent, func(), error) {
	return p.client.Stream(ctx, req, p.codec.EncodeRequest, p.decoder)
}

// RefreshCredentials is a no-op: custom providers authenticate with a
// static operator-supplied key.
func (p *Provider) RefreshCredentials(_ context.Context) error { return nil }

// ListModels fetches the upstream model list (ADR-010 model sync) —
// GET {baseURL}/models, the OpenAI list shape (fixture
// testdata/golden/openai/models.json, developers.openai.com, 2026-09-10).
// The entry "id" field is the wire model identifier.
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
		return nil, fmt.Errorf("custom: decode /models response: %w", err)
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

// PreviewRequest encodes the request exactly as Complete/Stream would send
// it (provider.RequestPreviewer): the Models page's test popup shows the
// operator what went out, not a paraphrase.
func (p *Provider) PreviewRequest(req ir.Request) ([]byte, error) {
	return p.codec.EncodeRequest(req)
}
