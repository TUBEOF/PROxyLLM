package firecrawl

// Firecrawl v2 client contract tests. Expectations derive exclusively from
// the Firecrawl v2 API facts and the golden fixtures under
// testdata/golden/firecrawl/
// (sourced from docs.firecrawl.dev, accessed 2026-09-10) — never from client
// code behavior.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ttestutil "github.com/tubeof/proxyllm/internal/translate/testutil"
)

// capture records one incoming request for assertions.
type capture struct {
	method string
	path   string
	auth   string
	ct     string
	body   string
}

func golden(t *testing.T, rel string) string {
	t.Helper()
	return string(ttestutil.LoadGolden(t, rel))
}

// newUpstream starts a server that serves body with status and records the
// single incoming request into c.
func newUpstream(t *testing.T, c *capture, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.method = r.Method
		c.path = r.URL.Path
		c.auth = r.Header.Get("Authorization")
		c.ct = r.Header.Get("Content-Type")
		c.body = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// 1. Search contract: POST {base}/v2/search, Bearer auth, the
// golden request body on the wire, response decoded per the golden fixture.
func TestSearch_Contract(t *testing.T) {
	var c capture
	srv := newUpstream(t, &c, http.StatusOK, golden(t, "firecrawl/search-response.json"))
	cl := New(srv.URL, "fc-test-key", nil)

	var req SearchRequest
	if err := json.Unmarshal([]byte(golden(t, "firecrawl/search-request.json")), &req); err != nil {
		t.Fatalf("decode golden search request: %v", err)
	}

	resp, err := cl.Search(context.Background(), req)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// Wire assertions per the documented v2 search shape.
	if c.method != http.MethodPost {
		t.Errorf("method = %q, want POST", c.method)
	}
	if c.path != "/v2/search" {
		t.Errorf("path = %q, want /v2/search", c.path)
	}
	if c.auth != "Bearer fc-test-key" {
		t.Errorf("Authorization = %q, want Bearer fc-test-key", c.auth)
	}
	if c.ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", c.ct)
	}

	// The encoded request must carry the golden fields.
	var sent map[string]any
	if err := json.Unmarshal([]byte(c.body), &sent); err != nil {
		t.Fatalf("sent body not JSON: %v\n%s", err, c.body)
	}
	if sent["query"] != "what is the current version of Go" {
		t.Errorf("query = %v, want golden query", sent["query"])
	}
	if sent["limit"] != float64(5) {
		t.Errorf("limit = %v, want 5", sent["limit"])
	}
	if got, ok := sent["scrapeOptions"].(map[string]any); !ok || got["formats"] == nil {
		t.Errorf("scrapeOptions.formats missing from %s", c.body)
	}

	// Response decoding per the golden fixture.
	if !resp.Success {
		t.Error("success = false, want true")
	}
	if len(resp.Data.Web) != 2 {
		t.Fatalf("web results = %d, want 2", len(resp.Data.Web))
	}
	w0 := resp.Data.Web[0]
	if w0.Title != "Downloads - The Go Programming Language" {
		t.Errorf("web[0].title = %q, want golden title", w0.Title)
	}
	if w0.URL != "https://go.dev/dl/" {
		t.Errorf("web[0].url = %q, want golden URL", w0.URL)
	}
	if !strings.Contains(w0.Markdown, "# Downloads") {
		t.Errorf("web[0].markdown missing content: %q", w0.Markdown)
	}
	if resp.Data.Web[1].Markdown != "" {
		t.Errorf("web[1].markdown = %q, want empty (fixture has none)", resp.Data.Web[1].Markdown)
	}
}

// 2. Scrape contract: POST {base}/v2/scrape with the golden request
// body; response markdown + metadata decoded per the golden fixture.
func TestScrape_Contract(t *testing.T) {
	var c capture
	srv := newUpstream(t, &c, http.StatusOK, golden(t, "firecrawl/scrape-response.json"))
	cl := New(srv.URL, "fc-test-key", nil)

	var req ScrapeRequest
	if err := json.Unmarshal([]byte(golden(t, "firecrawl/scrape-request.json")), &req); err != nil {
		t.Fatalf("decode golden scrape request: %v", err)
	}

	resp, err := cl.Scrape(context.Background(), req)
	if err != nil {
		t.Fatalf("Scrape: %v", err)
	}

	if c.method != http.MethodPost || c.path != "/v2/scrape" {
		t.Errorf("method/path = %q/%q, want POST /v2/scrape", c.method, c.path)
	}
	if c.auth != "Bearer fc-test-key" {
		t.Errorf("Authorization = %q, want Bearer fc-test-key", c.auth)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(c.body), &sent); err != nil {
		t.Fatalf("sent body not JSON: %v\n%s", err, c.body)
	}
	if sent["url"] != "https://go.dev/dl/" {
		t.Errorf("url = %v, want golden URL", sent["url"])
	}

	if !strings.Contains(resp.Data.Markdown, "Featured downloads") {
		t.Errorf("markdown = %q, want golden content", resp.Data.Markdown)
	}
	if resp.Data.Metadata.Title != "Downloads - The Go Programming Language" {
		t.Errorf("metadata.title = %q, want golden title", resp.Data.Metadata.Title)
	}
	if resp.Data.Metadata.URL != "https://go.dev/dl/" {
		t.Errorf("metadata.url = %q, want golden URL", resp.Data.Metadata.URL)
	}
	if resp.Data.Metadata.StatusCode != 200 {
		t.Errorf("metadata.statusCode = %d, want 200", resp.Data.Metadata.StatusCode)
	}
}

// 3. Default base URL is the production API host.
func TestNew_DefaultBaseURL(t *testing.T) {
	cl := New("", "fc-key", nil)
	if cl.BaseURL() != "https://api.firecrawl.dev" {
		t.Errorf("default BaseURL = %q, want https://api.firecrawl.dev", cl.BaseURL())
	}
}

// 4. Error semantics: non-2xx surfaces as a typed error carrying the
// status; the client never fabricates success from a failed response.
func TestSearch_ErrorSurfacesStatus(t *testing.T) {
	var c capture
	srv := newUpstream(t, &c, http.StatusTooManyRequests,
		`{"success":false,"error":"Plan rate limit exceeded"}`)
	cl := New(srv.URL, "fc-key", nil)

	_, err := cl.Search(context.Background(), SearchRequest{Query: "q"})
	if err == nil {
		t.Fatal("no error on 429 response")
	}
	var api *APIError
	if !errors.As(err, &api) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if api.HTTPStatus != http.StatusTooManyRequests {
		t.Errorf("HTTPStatus = %d, want 429", api.HTTPStatus)
	}
	if !strings.Contains(api.ErrorMsg, "rate limit") {
		t.Errorf("message = %q, want rate-limit text", api.ErrorMsg)
	}
}

// 5. Transport failures are distinct from API errors — the caller must be
// able to tell "Firecrawl answered no" from "Firecrawl unreachable".
func TestSearch_TransportErrorDistinct(t *testing.T) {
	var c capture
	srv := newUpstream(t, &c, http.StatusOK, `{}`)
	srv.Close() // unreachable now
	cl := New(srv.URL, "fc-key", nil)

	_, err := cl.Search(context.Background(), SearchRequest{Query: "q"})
	if err == nil {
		t.Fatal("no error on unreachable upstream")
	}
	var api *APIError
	if errors.As(err, &api) {
		t.Errorf("transport failure surfaced as APIError (status %d)", api.HTTPStatus)
	}
}

// 6. success:false on a 2xx is still an error — the flag is the documented
// contract, not the status code alone.
func TestScrape_SuccessFalseIsError(t *testing.T) {
	var c capture
	srv := newUpstream(t, &c, http.StatusOK, `{"success":false,"error":"scrape failed"}`)
	cl := New(srv.URL, "fc-key", nil)

	_, err := cl.Scrape(context.Background(), ScrapeRequest{URL: "https://example.com"})
	var api *APIError
	if !errors.As(err, &api) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if !strings.Contains(api.ErrorMsg, "scrape failed") {
		t.Errorf("message = %q, want scrape failed", api.ErrorMsg)
	}
}
