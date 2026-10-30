// Package ollamanative implements the native Ollama /api/chat provider:
// NDJSON streaming, the Ollama error shape, and /api/tags discovery. It is
// the native sibling of the OpenAI-compatible ollama-cloud provider.
package ollamanative

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
	"github.com/tubeof/proxyllm/internal/translate/ollama"
)

// DefaultBaseURL is a local Ollama server.
const DefaultBaseURL = "http://localhost:11434"

// Provider is one configured native Ollama endpoint.
type Provider struct {
	client httpx.Client
	id     string
	caps   provider.Capabilities
	codec  ollama.Codec
}

// New constructs the provider. An empty baseURL selects DefaultBaseURL.
func New(id, baseURL, apiKey string, caps provider.Capabilities, opts ...httpguard.ClientOption) *Provider {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	return &Provider{
		client: httpx.Client{
			BaseURL:    base,
			ChatPath:   "/api/chat",
			APIKey:     apiKey,
			ErrorShape: httpx.ErrorOllama,
			HTTP:       httpguard.NewStreamingClient(opts...),
			HeaderHook: func(ctx context.Context, _ bool) (http.Header, error) {
				return provider.MergeForwarded(ctx, make(http.Header)), nil
			},
		},
		id:    id,
		caps:  caps,
		codec: ollama.Codec{},
	}
}

// ID returns the stable provider identifier.
func (p *Provider) ID() string { return p.id }

// Type reports the canonical provider type.
func (p *Provider) Type() string { return "ollama-native" }

// Capabilities reports the operator-configured capability set.
func (p *Provider) Capabilities() provider.Capabilities { return p.caps }

// Complete performs one non-streaming /api/chat round trip.
func (p *Provider) Complete(ctx context.Context, req ir.Request) (ir.Response, error) {
	return p.client.Complete(ctx, req, p.codec.EncodeRequest, p.codec.DecodeResponse)
}

// Stream starts one streaming /api/chat round trip (NDJSON).
func (p *Provider) Stream(ctx context.Context, req ir.Request) (<-chan ir.StreamEvent, func(), error) {
	return p.client.Stream(ctx, req, p.codec.EncodeRequest, func(r io.Reader) translate.StreamDecoder {
		return ollama.NewStreamDecoder(r)
	})
}

// RefreshCredentials is a no-op: static API key (or none for a local server).
func (p *Provider) RefreshCredentials(_ context.Context) error { return nil }

// ListModels fetches /api/tags. The tag list carries no capability flags;
// the operator pins vision/thinking/tools per model where needed.
func (p *Provider) ListModels(ctx context.Context) ([]provider.ModelInfo, error) {
	raw, err := p.client.Get(ctx, "/api/tags")
	if err != nil {
		return nil, err
	}
	var list struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("ollama-native: decode /api/tags response: %w", err)
	}
	out := make([]provider.ModelInfo, 0, len(list.Models))
	seen := make(map[string]struct{}, len(list.Models))
	for _, m := range list.Models {
		if m.Name == "" {
			continue
		}
		if _, dup := seen[m.Name]; dup {
			continue
		}
		seen[m.Name] = struct{}{}
		out = append(out, provider.ModelInfo{Name: m.Name})
	}
	return out, nil
}

// PreviewRequest encodes the exact /api/chat body a call would send.
func (p *Provider) PreviewRequest(req ir.Request) ([]byte, error) {
	return p.codec.EncodeRequest(req)
}
