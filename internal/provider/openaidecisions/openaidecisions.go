// Package openaidecisions implements the OpenAI Decisions API provider:
// POST {base}/v1/decisions with Bearer authentication, non-streaming. It
// implements provider.Decider and is not a chat provider.
package openaidecisions

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/tubeof/proxyllm/internal/httpguard"
	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/provider/internal/httpx"
	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/openai"
)

// ErrNotChat is returned by Complete/Stream: this account serves only the
// decision surface.
var ErrNotChat = errors.New("openai-decisions: not a chat provider")

// Provider is one configured OpenAI Decisions endpoint.
type Provider struct {
	client httpx.Client
	id     string
	caps   provider.Capabilities
	codec  openai.DecisionsCodec
}

// New constructs the provider. baseURL is the API root (e.g.
// https://api.example.com); the /v1/decisions path rides the endpoint.
func New(id, baseURL, apiKey string, caps provider.Capabilities, opts ...httpguard.ClientOption) *Provider {
	return &Provider{
		client: httpx.Client{
			BaseURL:    strings.TrimRight(baseURL, "/"),
			ChatPath:   "/v1/decisions",
			APIKey:     apiKey,
			ErrorShape: httpx.ErrorOpenAI,
			HTTP:       httpguard.NewStreamingClient(opts...),
			HeaderHook: func(ctx context.Context, _ bool) (http.Header, error) {
				return provider.MergeForwarded(ctx, make(http.Header)), nil
			},
		},
		id:    id,
		caps:  caps,
		codec: openai.DecisionsCodec{},
	}
}

// ID returns the stable provider identifier.
func (p *Provider) ID() string { return p.id }

// Type reports the canonical provider type.
func (p *Provider) Type() string { return "openai-decisions" }

// Capabilities reports the decision-only capability set.
func (p *Provider) Capabilities() provider.Capabilities { return p.caps }

// Complete refuses chat: the account is decision-only.
func (p *Provider) Complete(_ context.Context, _ ir.Request) (ir.Response, error) {
	return ir.Response{}, ErrNotChat
}

// Stream refuses chat: the account is decision-only.
func (p *Provider) Stream(_ context.Context, _ ir.Request) (<-chan ir.StreamEvent, func(), error) {
	return nil, func() {}, ErrNotChat
}

// Decide performs one non-streaming decision round trip.
func (p *Provider) Decide(ctx context.Context, req ir.DecisionRequest) (ir.DecisionResponse, error) {
	raw, err := p.codec.EncodeRequest(req)
	if err != nil {
		return ir.DecisionResponse{}, fmt.Errorf("openai-decisions: encode request: %w", err)
	}
	body, err := p.client.PostJSON(ctx, "/v1/decisions", raw)
	if err != nil {
		return ir.DecisionResponse{}, err
	}
	resp, err := p.codec.DecodeResponse(body)
	if err != nil {
		return ir.DecisionResponse{}, fmt.Errorf("openai-decisions: decode response: %w", err)
	}
	return resp, nil
}

// RefreshCredentials is a no-op: static API key.
func (p *Provider) RefreshCredentials(_ context.Context) error { return nil }
