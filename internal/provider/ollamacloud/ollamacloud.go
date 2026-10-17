// Package ollamacloud implements the Ollama Cloud provider:
// the hosted OpenAI-compatible API at https://ollama.com/v1. It speaks the
// OpenAI chat-completions wire through the OpenAI codec and the
// shared HTTP transport foundation. The OpenAI-compat surface is the
// cloud API's primary chat transport (docs.ollama.com/api/
// openai-compatibility — cloud access is base_url https://ollama.com/v1
// with an OLLAMA_API_KEY bearer); the native /api/chat wire fragmented
// interleaved thinking traces into separate content blocks (a Claude Code
// rendering bug), which the OpenAI surface avoids.
// Local Ollama servers attach via the custom-openai provider type.
package ollamacloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strings"

	"github.com/tubeof/proxyllm/internal/httpguard"
	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/provider/internal/httpx"
	"github.com/tubeof/proxyllm/internal/translate"
	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/openai"
	"github.com/tubeof/proxyllm/internal/translate/systemone"
)

// DefaultBaseURL is the hosted Ollama Cloud API root. The /v1
// version prefix rides the endpoint paths (custom-openai convention).
const DefaultBaseURL = "https://ollama.com"

// SystemOnePath is the decision endpoint (System One / Jev) relative to
// the base URL: the hosted API (https://ollama.com/v1/systemone) and a
// local Ollama server (http://localhost:11434/v1/systemone) serve the same
// path, so the provider reaches `clef:cloud` and a locally loaded clef
// through one code path.
const SystemOnePath = "/v1/systemone"

// Provider is the Ollama Cloud provider: the OpenAI-compatible
// /v1/chat/completions over HTTPS with Bearer auth, data-only SSE
// streaming.
type Provider struct {
	client  httpx.Client
	id      string
	caps    provider.Capabilities
	codec   openai.Codec
	decoder func(r io.Reader) translate.StreamDecoder
	logger  *slog.Logger // nil = silent (tests); enrichment failures log here
}

// New constructs the Ollama Cloud provider. baseURL defaults to
// DefaultBaseURL when empty.
func New(id, baseURL, apiKey string, caps provider.Capabilities) *Provider {
	return NewWithLogger(id, baseURL, apiKey, caps, nil)
}

// NewWithLogger is New with a logger for detection-enrichment failures
// (best-effort probes degrade per model — but never silently).
func NewWithLogger(id, baseURL, apiKey string, caps provider.Capabilities, logger *slog.Logger) *Provider {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Provider{
		client: httpx.Client{
			BaseURL:    baseURL,
			ChatPath:   "/v1/chat/completions", // OpenAI-compat endpoint
			APIKey:     apiKey,                 // Bearer $OLLAMA_API_KEY
			ErrorShape: httpx.ErrorOpenAI,
			// Streaming transport: no total-body budget (long generations
			// must not be cut off), but redirects re-validated against the
			// private-address SSRF guard — the Bearer header would ride a
			// followed redirect.
			HTTP: httpguard.NewStreamingClient(),
		},
		id:      id,
		caps:    caps,
		codec:   openai.Codec{},
		decoder: func(r io.Reader) translate.StreamDecoder { return openai.NewStreamDecoder(r) },
		logger:  logger,
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
// body carries an explicit "stream":false — the codec emits it.
func (p *Provider) Complete(ctx context.Context, req ir.Request) (ir.Response, error) {
	return p.client.Complete(ctx, req, p.codec.EncodeRequest, p.codec.DecodeResponse)
}

// Stream starts one streaming /api/chat round trip (NDJSON lines).
func (p *Provider) Stream(ctx context.Context, req ir.Request) (<-chan ir.StreamEvent, func(), error) {
	return p.client.Stream(ctx, req, p.codec.EncodeRequest, p.decoder)
}

// Decide performs one decision round trip (clef and the other System One
// models) against {BaseURL}/v1/systemone with the account's Bearer key —
// the same credential the chat path uses. The wire pair is the systemone
// codec: non-streaming, one JSON response; upstream errors carry the
// account's error shape and classify like any other upstream failure.
func (p *Provider) Decide(ctx context.Context, req ir.DecisionRequest) (ir.DecisionResponse, error) {
	raw, err := systemone.Codec{}.EncodeRequest(req)
	if err != nil {
		return ir.DecisionResponse{}, fmt.Errorf("ollamacloud: encode decision request: %w", err)
	}
	body, err := p.client.PostJSON(ctx, SystemOnePath, raw)
	if err != nil {
		return ir.DecisionResponse{}, err
	}
	resp, err := systemone.Codec{}.DecodeResponse(body)
	if err != nil {
		return ir.DecisionResponse{}, fmt.Errorf("ollamacloud: decode decision response: %w", err)
	}
	return resp, nil
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
		info := provider.ModelInfo{Name: m.ID}
		// Per-model enrichment, best-effort: a failed /api/show probe
		// leaves the zero-value facts — the model still imports (never a
		// sync-run failure). The failure is never silent: it lands in the
		// sync log so an operator seeing all-zero facts can tell a failing
		// registry from genuinely capability-less models.
		if shown, err := p.showModel(ctx, m.ID); err == nil {
			info.ContextLength = shown.contextLength()
			info.Thinking = shown.hasCapability("thinking")
			info.Tools = shown.hasCapability("tools")
			info.Vision = shown.hasCapability("vision")
			info.Decision = shown.hasCapability("decision")
		} else if p.logger != nil {
			p.logger.Debug("model sync: /api/show detection probe failed — importing with zero facts",
				"provider_id", p.id, "model", m.ID, "error", err)
		}
		out = append(out, info)
	}
	return out, nil
}

// showModel POSTs /api/show {"model": id} — the Ollama Cloud registry's
// per-model facts endpoint (verified live 2026-10-06; unauthenticated
// calls work, but the Bearer rides anyway for hosts that gate it).
func (p *Provider) showModel(ctx context.Context, model string) (showResponse, error) {
	body, err := json.Marshal(map[string]string{"model": model})
	if err != nil {
		return showResponse{}, fmt.Errorf("ollamacloud: encode /api/show request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.client.BaseURL+"/api/show", bytes.NewReader(body))
	if err != nil {
		return showResponse{}, fmt.Errorf("ollamacloud: build /api/show request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.client.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.HTTP.Do(req)
	if err != nil {
		return showResponse{}, fmt.Errorf("ollamacloud: /api/show: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return showResponse{}, fmt.Errorf("ollamacloud: read /api/show body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return showResponse{}, fmt.Errorf("ollamacloud: /api/show %s: HTTP %d", model, resp.StatusCode)
	}
	var out showResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return showResponse{}, fmt.Errorf("ollamacloud: decode /api/show body: %w", err)
	}
	return out, nil
}

// showResponse is the /api/show subset relevant to detection. model_info
// carries heterogeneous values (strings like "general.architecture",
// numerics like parameter counts) and is decoded as raw messages — the
// context_length value parses per matching key below. The context_length
// key's arch prefix varies per model family ("kimi-k2.context_length",
// "glm5_next.context_length", …) — it is parsed from ANY model_info key
// with that suffix. Capability name strings ("completion", "thinking",
// "tools", "vision", "decision") are Ollama's registry wire facts, mapped
// below like other vendor-specific mappings: "decision" is the per-model
// declaration of the decision surface (clef:cloud, the System One / Jev
// class), verified live 2026-10-16.
type showResponse struct {
	ModelInfo    map[string]json.RawMessage `json:"model_info"`
	Capabilities []string                   `json:"capabilities"`
	Error        string                     `json:"error"`
}

// contextLength returns any "<arch>.context_length" numeric value from
// model_info. Ties on multiple matching keys resolve deterministically on
// the lexicographically first key (determinism contract).
func (s showResponse) contextLength() int {
	bestKey := ""
	var bestVal int64
	for k, raw := range s.ModelInfo {
		if !strings.HasSuffix(k, "context_length") {
			continue
		}
		var v int64
		if err := json.Unmarshal(raw, &v); err != nil || v <= 0 {
			continue
		}
		if bestKey == "" || k < bestKey {
			bestKey, bestVal = k, v
		}
	}
	if bestVal > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(bestVal)
}

// hasCapability reports whether the capabilities list names cap.
func (s showResponse) hasCapability(cap string) bool {
	return slices.Contains(s.Capabilities, cap)
}

// RefreshCredentials is a no-op: Ollama Cloud authenticates with a static
// operator-supplied API key.
func (p *Provider) RefreshCredentials(_ context.Context) error { return nil }
