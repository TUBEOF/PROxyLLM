// Package firecrawl implements the Firecrawl v2 REST client (facts F1-F3):
// the universal web-tool fallback behind the web_search/web_fetch emulation
// layer (plan §6.2). There is no official Go SDK (F5), so this is a direct
// JSON-over-HTTP client against the documented endpoints.
package firecrawl

import (
	"bytes"
	"context"
	"encoding/json"

	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL is the production Firecrawl API host (F1).
const DefaultBaseURL = "https://api.firecrawl.dev"

// APIError is a Firecrawl answer in the negative — HTTP status and the
// service's own error text. Transport failures (unreachable upstream) are
// NOT APIError, so callers can tell "answered no" from "unreachable".
type APIError struct {
	HTTPStatus int // 0 never occurs on APIError
	Type       string
	ErrorMsg   string
}

func (e *APIError) Error() string {
	if e.Type != "" {
		return fmt.Sprintf("firecrawl: HTTP %d %s: %s", e.HTTPStatus, e.Type, e.ErrorMsg)
	}
	return fmt.Sprintf("firecrawl: HTTP %d: %s", e.HTTPStatus, e.ErrorMsg)
}

// SearchRequest is the /v2/search body (F2). Field tags match the
// documented parameter names verbatim — scrapeOptions stays camelCase on
// the wire even though Go style says otherwise.
type SearchRequest struct {
	Query          string         `json:"query"`
	Limit          int            `json:"limit,omitempty"`
	Sources        []string       `json:"sources,omitempty"`
	Categories     []string       `json:"categories,omitempty"`
	IncludeDomains []string       `json:"includeDomains,omitempty"`
	ExcludeDomains []string       `json:"excludeDomains,omitempty"`
	ScrapeOptions  *ScrapeOptions `json:"scrapeOptions,omitempty"`
	TBS            string         `json:"tbs,omitempty"`
	Country        string         `json:"country,omitempty"`
}

// ScrapeOptions is the nested scrapeOptions object (F2).
type ScrapeOptions struct {
	Formats []string `json:"formats,omitempty"`
}

// ScrapeRequest is the /v2/scrape body (F3).
type ScrapeRequest struct {
	URL     string   `json:"url"`
	Formats []string `json:"formats,omitempty"`
}

// SearchResponse mirrors the golden search-response fixture shape (F2):
// data.web[] with title/url/description and optional markdown.
type SearchResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Web []WebResult `json:"web"`
	} `json:"data"`
	Warning []string `json:"warning"`
}

// WebResult is one entry of data.web[] (F2). Markdown is present only when
// scrapeOptions requested it — absence is not an error.
type WebResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
	Markdown    string `json:"markdown"`
}

// ScrapeResponse mirrors the golden scrape-response fixture shape (F3).
type ScrapeResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Markdown string `json:"markdown"`
		Metadata struct {
			Title      string `json:"title"`
			URL        string `json:"url"`
			StatusCode int    `json:"statusCode"`
		} `json:"metadata"`
	} `json:"data"`
}

// Client calls the Firecrawl v2 API. All requests carry
// Authorization: Bearer fc-... (F1) and a bounded timeout.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New builds a client. An empty baseURL selects DefaultBaseURL; a nil HTTP
// client selects a default with a bounded timeout (emulation runs inline in
// the request path, so unbounded waits cannot wedge a proxy request).
func New(baseURL, apiKey string, httpClient *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{baseURL: strings.TrimSuffix(baseURL, "/"), apiKey: apiKey, http: httpClient}
}

// BaseURL returns the configured API host (test seam for the default-host
// assertion; not a config surface).
func (c *Client) BaseURL() string { return c.baseURL }

// Search calls POST /v2/search (F2).
func (c *Client) Search(ctx context.Context, req SearchRequest) (*SearchResponse, error) {
	var out SearchResponse
	if err := c.call(ctx, "/v2/search", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Scrape calls POST /v2/scrape (F3).
func (c *Client) Scrape(ctx context.Context, req ScrapeRequest) (*ScrapeResponse, error) {
	var out ScrapeResponse
	if err := c.call(ctx, "/v2/scrape", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// maxResponseBytes caps response bodies at 4 MB. Unlike the httpx 64 KB
// error-body cap, this bounds *success* payloads — full-page scrape
// markdown routinely exceeds tens of KB — while still preventing unbounded
// proxy memory on a hostile or broken upstream.
const maxResponseBytes = 4 << 20

// call posts one JSON request and decodes the response. Non-2xx statuses
// map to *APIError; transport failures are returned unwrapped; success:false
// on a 2xx is an APIError too — the flag is the documented contract.
func (c *Client) call(ctx context.Context, path string, req any, out successResponse) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("firecrawl: encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("firecrawl: build request: %w", err)
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
		return fmt.Errorf("firecrawl: read response body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeAPIError(resp.StatusCode, respBody)
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("firecrawl: decode response: %w", err)
	}
	return out.checkSuccess(respBody)
}

// successResponse is implemented by response types carrying the
// documented success flag.
type successResponse interface {
	// checkSuccess enforces the documented success flag on a 2xx body;
	// raw is the undecoded response body, used to surface the service's
	// own error text when success is false.
	checkSuccess(raw []byte) error
}

func (r *SearchResponse) checkSuccess(raw []byte) error {
	if r.Success {
		return nil
	}
	return successFalseError("search", raw)
}

func (r *ScrapeResponse) checkSuccess(raw []byte) error {
	if r.Success {
		return nil
	}
	return successFalseError("scrape", raw)
}

// successFalseError builds the APIError for a success:false 2xx body,
// preferring the service's own error text over a generic message.
func successFalseError(op string, raw []byte) error {
	apiErr := &APIError{ErrorMsg: op + " returned success:false"}
	var wire struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &wire); err == nil && wire.Error != "" {
		apiErr.ErrorMsg = wire.Error
	}
	return apiErr
}

// decodeAPIError parses the Firecrawl error body shape: the docs show
// success:false with a plain string "error" field (F5).
func decodeAPIError(status int, body []byte) *APIError {
	apiErr := &APIError{HTTPStatus: status}
	var wire struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &wire); err == nil && wire.Error != "" {
		apiErr.ErrorMsg = wire.Error
	} else {
		apiErr.ErrorMsg = string(body) // bounded by LimitReader upstream
	}
	return apiErr
}
