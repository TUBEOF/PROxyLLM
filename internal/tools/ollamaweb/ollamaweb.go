// Package ollamaweb implements Ollama's hosted web-tool REST API — the
// second web_search/web_fetch backend behind the emulation layer, alongside
// Firecrawl. Both endpoints are plain JSON-over-HTTP against ollama.com and
// authenticate with the same Bearer key as the ollama-cloud chat provider.
//
// Facts verified against the official docs on 2026-10-11
// (https://docs.ollama.com/capabilities/web-search):
//
//   - POST /api/web_search, body {query, max_results}; max_results defaults
//     to 5 and caps at 10. Answer: {results:[{title,url,content}]}.
//   - POST /api/web_fetch, body {url}. Answer: {title,content,links[]}.
//   - A free Ollama account is enough; the key rides Authorization: Bearer.
//
// Two documented gaps against Firecrawl, both surfaced honestly rather than
// papered over: the search endpoint takes no domain filters, and its per-result
// `content` snippet has no field in the IR (see Emulator for both).
package ollamaweb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tubeof/proxyllm/internal/httpguard"
)

// DefaultBaseURL is the hosted Ollama API host.
const DefaultBaseURL = "https://ollama.com"

// APIError is an Ollama answer in the negative — HTTP status and, when the
// body carries one, the service's own error text. Transport failures are NOT
// APIError, so callers can tell "answered no" from "unreachable".
type APIError struct {
	HTTPStatus int // 0 never occurs on APIError
	ErrorMsg   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ollamaweb: HTTP %d: %s", e.HTTPStatus, e.ErrorMsg)
}

// SearchRequest is the /api/web_search body. MaxResults is omitted when zero
// so the service's documented default (5) applies; the emulation layer has no
// result-count knob of its own.
type SearchRequest struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results,omitempty"`
}

// SearchResponse is the documented /api/web_search 200 body.
type SearchResponse struct {
	Results []SearchResult `json:"results"`
}

// SearchResult is one entry of results[]. Content is the page snippet the
// service provides; the IR has no field for it (see Emulator.Search).
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

// FetchRequest is the /api/web_fetch body.
type FetchRequest struct {
	URL string `json:"url"`
}

// FetchResponse is the documented /api/web_fetch 200 body.
type FetchResponse struct {
	Title   string   `json:"title"`
	Content string   `json:"content"`
	Links   []string `json:"links"`
}

// Client calls the Ollama web-tool API. All requests carry
// Authorization: Bearer and a bounded timeout.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New builds a client. An empty baseURL selects DefaultBaseURL; a nil HTTP
// client selects a guarded default with a bounded timeout (emulation runs
// inline in the request path, so unbounded waits cannot wedge a proxy request).
func New(baseURL, apiKey string, httpClient *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if httpClient == nil {
		httpClient = httpguard.NewClient(0)
	}
	return &Client{baseURL: strings.TrimSuffix(baseURL, "/"), apiKey: apiKey, http: httpClient}
}

// BaseURL returns the configured API host (test seam, not a config surface).
func (c *Client) BaseURL() string { return c.baseURL }

// Search calls POST /api/web_search.
func (c *Client) Search(ctx context.Context, req SearchRequest) (*SearchResponse, error) {
	var out SearchResponse
	if err := c.call(ctx, "/api/web_search", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Fetch calls POST /api/web_fetch.
func (c *Client) Fetch(ctx context.Context, req FetchRequest) (*FetchResponse, error) {
	var out FetchResponse
	if err := c.call(ctx, "/api/web_fetch", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// maxResponseBytes caps response bodies at 4 MB, mirroring the Firecrawl
// client's bound: a fetched page routinely exceeds tens of KB, while the cap
// still prevents unbounded proxy memory on a hostile or broken upstream.
const maxResponseBytes = 4 << 20

// call posts one JSON request and decodes the response. Non-2xx statuses map
// to *APIError; transport failures are returned unwrapped.
func (c *Client) call(ctx context.Context, path string, req, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("ollamaweb: encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("ollamaweb: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return err // transport failure: no APIError
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("ollamaweb: read response body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeAPIError(resp.StatusCode, respBody)
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("ollamaweb: decode response: %w", err)
	}
	return nil
}

// decodeAPIError builds the APIError for a non-2xx answer. The body is not
// echoed: it is untrusted content that could otherwise flow into log rows or
// client-visible error messages. Size and status are enough to debug from.
func decodeAPIError(status int, body []byte) *APIError {
	apiErr := &APIError{HTTPStatus: status}
	var wire struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &wire); err == nil && wire.Error != "" {
		apiErr.ErrorMsg = wire.Error
	} else {
		apiErr.ErrorMsg = fmt.Sprintf("non-JSON error body (HTTP %d, %d bytes)", status, len(body))
	}
	return apiErr
}
