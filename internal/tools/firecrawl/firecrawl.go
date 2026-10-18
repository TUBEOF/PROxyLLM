// Package firecrawl implements the Firecrawl v2 REST client:
// the universal web-tool fallback behind the web_search/web_fetch emulation
// layer. There is no official Go SDK, so this is a direct
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

	"github.com/tubeof/proxyllm/internal/httpguard"
)

// DefaultBaseURL is the production Firecrawl API host.
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

// SearchRequest is the /v2/search body. Field tags match the
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

// ScrapeOptions is the nested scrapeOptions object.
type ScrapeOptions struct {
	Formats []string `json:"formats,omitempty"`
}

// ScrapeRequest is the /v2/scrape body.
type ScrapeRequest struct {
	URL     string   `json:"url"`
	Formats []string `json:"formats,omitempty"`
}

// SearchResponse mirrors the documented /v2/search 200 body (re-read
// 2026-10-10): success, data.web[], and a nullable top-level warning. The
// arrays present depend on the sources asked for; web is the default.
type SearchResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Web []WebResult `json:"web"`
	} `json:"data"`

	// Warning is informational (partial source degradation with success:true),
	// never a failure. See WarningText for why it is not a plain string.
	Warning WarningText `json:"warning"`
}

// WarningText is Firecrawl's `warning` (and `metadata.error`) field. The v2
// reference types both as a nullable string — search's warning at the top
// level, scrape's under data — while the fixture this repo captured earlier
// carried an empty array instead. Decoding is therefore tolerant of a string,
// an array of strings, and null:
//
// a warning is a soft signal about an otherwise successful answer, and a type
// mismatch on it must not turn that answer into a decode error (which is how a
// non-empty warning would have failed a whole scrape, since an unparseable body
// is a transport-level failure to the caller).
type WarningText string

// UnmarshalJSON accepts string | []string | null | anything else, because the
// field is informational and its shape has differed between the docs and the
// wire. An unrecognised shape is kept as its raw text rather than dropped.
func (w *WarningText) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" || trimmed == "null" {
		*w = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*w = WarningText(s)
		return nil
	}
	var list []string
	if err := json.Unmarshal(b, &list); err == nil {
		*w = WarningText(strings.Join(list, "; "))
		return nil
	}
	*w = WarningText(strings.Trim(trimmed, `"`))
	return nil
}

// WebResult is one entry of data.web[]. Markdown is present only when
// scrapeOptions requested it — absence is not an error.
type WebResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
	Markdown    string `json:"markdown"`
}

// ScrapeResponse mirrors the documented /v2/scrape 200 body (re-read
// 2026-10-10): success, data.markdown, and metadata carrying the page's own
// status code and error note. There is no top-level warning here — scrape's
// warning rides under data.
type ScrapeResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Markdown string      `json:"markdown"`
		Warning  WarningText `json:"warning"`
		Metadata struct {
			Title      string `json:"title"`
			URL        string `json:"url"`
			SourceURL  string `json:"sourceURL"`
			StatusCode int    `json:"statusCode"`
			// Error is the page's own error ("The error message of the page"),
			// which is how a 200 response reports a page it could not serve.
			Error WarningText `json:"error"`
		} `json:"metadata"`
	} `json:"data"`
}

// CreditUsageResponse mirrors the documented GET /v2/team/credit-usage 200
// body (re-read 2026-10-18):
//
//	{"success":true,"data":{"remainingCredits":1000,"planCredits":500000,
//	 "billingPeriodStart":"...","billingPeriodEnd":"..."}}
//
// The credit counts are pointers so an absent field stays "unknown" rather
// than becoming a fabricated zero. 404 and 500 bodies are success:false with
// an error string and decode through the same path as search/scrape.
type CreditUsageResponse struct {
	Success bool `json:"success"`
	Data    struct {
		RemainingCredits   *float64 `json:"remainingCredits"`
		PlanCredits        *float64 `json:"planCredits"`
		BillingPeriodStart string   `json:"billingPeriodStart"`
		BillingPeriodEnd   string   `json:"billingPeriodEnd"`
	} `json:"data"`
}

func (r *CreditUsageResponse) checkSuccess(raw []byte) error {
	if r.Success {
		return nil
	}
	return successFalseError("credit-usage", raw)
}

// Client calls the Firecrawl v2 API. All requests carry
// Authorization: Bearer fc-... and a bounded timeout.
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
		// Guarded default: same 30s total budget as before, with redirects
		// re-validated against the private-address SSRF guard — the
		// Bearer fc-... key rides a followed redirect.
		httpClient = httpguard.NewClient(0)
	}
	return &Client{baseURL: strings.TrimSuffix(baseURL, "/"), apiKey: apiKey, http: httpClient}
}

// BaseURL returns the configured API host (test seam for the default-host
// assertion; not a config surface).
func (c *Client) BaseURL() string { return c.baseURL }

// Search calls POST /v2/search.
func (c *Client) Search(ctx context.Context, req SearchRequest) (*SearchResponse, error) {
	var out SearchResponse
	if err := c.call(ctx, "/v2/search", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Scrape calls POST /v2/scrape.
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
	return c.do(ctx, http.MethodPost, path, body, out)
}

// get performs one GET and decodes the response through the same error
// contract as call. The credit-usage endpoint is the first GET surface.
func (c *Client) get(ctx context.Context, path string, out successResponse) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// do is the one request path: the Authorization header, the bounded body
// read, the non-2xx mapping and the success-flag check are identical for
// every endpoint.
func (c *Client) do(ctx context.Context, method, path string, body []byte, out successResponse) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("firecrawl: build request: %w", err)
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
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

// CreditUsage calls GET /v2/team/credit-usage — the account's remaining
// credits and billing period. A success:false body (documented for 404/500)
// is an APIError; the caller stores an honest error blob, never a zero.
func (c *Client) CreditUsage(ctx context.Context) (*CreditUsageResponse, error) {
	var out CreditUsageResponse
	if err := c.get(ctx, "/v2/team/credit-usage", &out); err != nil {
		return nil, err
	}
	return &out, nil
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
// success:false with a plain string "error" field.
func decodeAPIError(status int, body []byte) *APIError {
	apiErr := &APIError{HTTPStatus: status}
	var wire struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &wire); err == nil && wire.Error != "" {
		apiErr.ErrorMsg = wire.Error
	} else {
		// Never echo a raw upstream body: it is untrusted content that
		// could flow into log rows or client-visible error messages.
		// Size + status only — safe, and enough to debug from.
		apiErr.ErrorMsg = fmt.Sprintf("non-JSON error body (HTTP %d, %d bytes)", status, len(body))
	}
	return apiErr
}
