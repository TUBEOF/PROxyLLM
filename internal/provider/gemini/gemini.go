// Package gemini implements the native Gemini v1beta REST provider: POST
// /v1beta/models/{model}:generateContent (chat) and
// :streamGenerateContent?alt=sse (streaming) with x-goog-api-key
// authentication, :countTokens and the paginated /v1beta/models catalog. It
// reuses the gemini codec pair and the shared guarded HTTP transport.
package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/tubeof/proxyllm/internal/httpguard"
	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/provider/internal/httpx"
	"github.com/tubeof/proxyllm/internal/translate"
	xgemini "github.com/tubeof/proxyllm/internal/translate/gemini"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// DefaultBaseURL is the Google generative language endpoint.
const DefaultBaseURL = "https://generativelanguage.googleapis.com"

// maxAPIBodyBytes caps a non-streaming Gemini API body (models catalog and
// countTokens reply). Same bound as the shared transport's chat cap.
const maxAPIBodyBytes = 16 << 20

// Model catalog naming: the resource name is models/{model} — chat paths
// take the bare model.
const modelsResourcePrefix = "models/"

// modelsPageSize is the list page size (the reference documents a 1000 cap).
const modelsPageSize = 1000

// maxModelPages guards the pager against a hostile nextPageToken loop.
const maxModelPages = 100

// Provider is one configured Gemini account.
type Provider struct {
	client httpx.Client
	id     string
	caps   provider.Capabilities
	codec  xgemini.Codec
}

// New constructs the provider. The chat paths ride the endpoint:
// /v1beta/models/{model}:generateContent and :streamGenerateContent?alt=sse.
func New(id, baseURL, apiKey string, caps provider.Capabilities, opts ...httpguard.ClientOption) *Provider {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	return &Provider{
		client: httpx.Client{
			BaseURL: base,
			// ChatPath is per-request (chat paths name the model).
			ChatPath:   "",
			APIKey:     "",
			ErrorShape: httpx.ErrorGemini,
			HTTP:       httpguard.NewStreamingClient(opts...),
			HeaderHook: func(ctx context.Context, stream bool) (http.Header, error) {
				h := provider.MergeForwarded(ctx, make(http.Header))
				if apiKey != "" && h.Get("x-goog-api-key") == "" {
					h.Set("x-goog-api-key", apiKey)
				}
				if stream {
					h.Set("Accept", "text/event-stream")
				}
				return h, nil
			},
		},
		id:    id,
		caps:  caps,
		codec: xgemini.Codec{},
	}
}

// ID returns the stable provider identifier.
func (p *Provider) ID() string { return p.id }

// Type reports the canonical provider type.
func (p *Provider) Type() string { return "gemini" }

// Capabilities reports what the operator configured for this account.
func (p *Provider) Capabilities() provider.Capabilities { return p.caps }

// generateContentPath is the chat path for one model.
func (p *Provider) generateContentPath(model string) string {
	return "/v1beta/models/" + url.PathEscape(model) + ":generateContent"
}

// streamGenerateContentPath is the streaming path for one model, SSE-framed.
func (p *Provider) streamGenerateContentPath(model string) string {
	return "/v1beta/models/" + url.PathEscape(model) + ":streamGenerateContent?alt=sse"
}

// countTokensPath is the token counting path for one model.
func (p *Provider) countTokensPath(model string) string {
	return "/v1beta/models/" + url.PathEscape(model) + ":countTokens"
}

// Complete performs one non-streaming generateContent round trip.
func (p *Provider) Complete(ctx context.Context, req ir.Request) (ir.Response, error) {
	c := p.client
	c.ChatPath = p.generateContentPath(req.Model)
	return c.Complete(ctx, req, p.codec.EncodeRequest, p.codec.DecodeResponse)
}

// Stream starts one streaming generateContent round trip (alt=sse).
func (p *Provider) Stream(ctx context.Context, req ir.Request) (<-chan ir.StreamEvent, func(), error) {
	c := p.client
	c.ChatPath = p.streamGenerateContentPath(req.Model)
	return c.Stream(ctx, req, p.codec.EncodeRequest, func(r io.Reader) translate.StreamDecoder {
		return xgemini.NewStreamDecoder(r)
	})
}

// RefreshCredentials is a no-op: the provider uses a static API key.
func (p *Provider) RefreshCredentials(_ context.Context) error { return nil }

// ListModels fetches the paginated /v1beta/models catalog. The Model
// resource carries a `thinking` boolean, which becomes ModelInfo.Thinking;
// tools/vision are the generateContent baseline (per-model rows can be
// pinned otherwise) and inputTokenLimit feeds the context window.
func (p *Provider) ListModels(ctx context.Context) ([]provider.ModelInfo, error) {
	var out []provider.ModelInfo
	pageToken := ""
	for page := 0; page < maxModelPages; page++ {
		path := "/v1beta/models?pageSize=" + strconv.Itoa(modelsPageSize)
		if pageToken != "" {
			path += "&pageToken=" + url.QueryEscape(pageToken)
		}
		raw, err := p.apiGet(ctx, path)
		if err != nil {
			return nil, err
		}
		var list struct {
			Models        []modelResource `json:"models"`
			NextPageToken string          `json:"nextPageToken"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("gemini: decode /v1beta/models response: %w", err)
		}
		for _, m := range list.Models {
			name := strings.TrimPrefix(m.Name, modelsResourcePrefix)
			if name == "" {
				continue
			}
			dup := false
			for _, existing := range out {
				if existing.Name == name {
					dup = true
					break
				}
			}
			if dup {
				continue
			}
			usable := false
			for _, method := range m.SupportedGenerationMethods {
				if method == "generateContent" {
					usable = true
					break
				}
			}
			// Models that cannot generateContent (embedding models) are not
			// routable chat models — the type has no model rows for them.
			if !usable {
				continue
			}
			out = append(out, provider.ModelInfo{
				Name:          name,
				ContextLength: m.InputTokenLimit,
				Thinking:      m.Thinking,
				Tools:         true,
				Vision:        true,
			})
		}
		if list.NextPageToken == "" {
			return out, nil
		}
		pageToken = list.NextPageToken
	}
	return out, fmt.Errorf("gemini: /v1beta/models did not terminate after %d pages", maxModelPages)
}

// CountTokens sends the full conversation as a countTokens
// generateContentRequest (contents[] and generateContentRequest are mutually
// exclusive) and returns totalTokens.
func (p *Provider) CountTokens(ctx context.Context, req ir.Request) (int, error) {
	body, err := p.codec.EncodeRequest(req)
	if err != nil {
		return 0, fmt.Errorf("gemini: encode countTokens request: %w", err)
	}
	wrapped, err := xgemini.EncodeCountTokensRequest(body)
	if err != nil {
		return 0, err
	}
	raw, err := p.apiPost(ctx, p.countTokensPath(req.Model), wrapped)
	if err != nil {
		return 0, err
	}
	return xgemini.DecodeCountTokensResponse(raw)
}

// PreviewRequest encodes the exact generateContent body a call would send.
func (p *Provider) PreviewRequest(req ir.Request) ([]byte, error) {
	return p.codec.EncodeRequest(req)
}

// ---- shared API transport (models catalog + countTokens) ----
//
// The chat paths go through the shared httpx transport; the non-chat GET/POST
// endpoints need the x-goog-api-key header without a Bearer — httpx.Get sends
// an unconditional Bearer — so these two helpers keep header control local.

// apiGet performs one authenticated GET round trip against a path relative to
// BaseURL.
func (p *Provider) apiGet(ctx context.Context, path string) ([]byte, error) {
	return p.apiRequest(ctx, http.MethodGet, path, nil)
}

// apiPost performs one authenticated POST round trip.
func (p *Provider) apiPost(ctx context.Context, path string, body []byte) ([]byte, error) {
	return p.apiRequest(ctx, http.MethodPost, path, body)
}

func (p *Provider) apiRequest(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = strings.NewReader(string(body))
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, p.client.BaseURL+path, rdr)
	if err != nil {
		return nil, fmt.Errorf("gemini: build request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	// The New HeaderHook applies x-goog-api-key (the Gemini auth header,
	// never Bearer) and merges forwarded headers.
	if p.client.HeaderHook != nil {
		extra, err := p.client.HeaderHook(ctx, false)
		if err != nil {
			return nil, fmt.Errorf("gemini: build provider headers: %w", err)
		}
		for name, vals := range extra {
			httpReq.Header[name] = vals
		}
	}
	resp, err := p.client.HTTP.Do(httpReq)
	if err != nil {
		return nil, err // transport failure: no UpstreamError (tier 1)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, mapAPIError(resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("gemini: read response body: %w", err)
	}
	if int64(len(b)) > maxAPIBodyBytes {
		return nil, fmt.Errorf("gemini: response body exceeds %d bytes", maxAPIBodyBytes)
	}
	return b, nil
}

// mapAPIError converts a non-2xx Gemini API response into the typed upstream
// error (the same two tiers the shared transport produces).
func mapAPIError(resp *http.Response) *provider.UpstreamError {
	up := &provider.UpstreamError{HTTPStatus: resp.StatusCode}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return up
	}
	var w struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &w) == nil {
		up.Message = w.Error.Message
		up.Type = w.Error.Status
	}
	if up.Message == "" && len(raw) > 0 {
		up.Message = strings.TrimSpace(string(raw))
	}
	return up
}

// modelResource mirrors one Model resource of the ListModelsResponse.
type modelResource struct {
	Name string `json:"name"`
	// Thinking is the resource's own "whether the model supports thinking"
	// flag — the discovery source for ModelInfo.Thinking.
	Thinking                   bool     `json:"thinking"`
	InputTokenLimit            int      `json:"inputTokenLimit"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
}
