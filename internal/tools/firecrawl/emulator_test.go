package firecrawl

// Emulator adapter contract tests (RED-GREEN per Analysis F). The adapter
// implements routing.Emulator (plan §6.2) on top of the v2 client. All
// expectations derive from plan facts F1-F3, the golden fixtures under
// testdata/golden/firecrawl/ (docs.firecrawl.dev, accessed 2026-09-10), and
// the routing.Emulator contract in internal/routing/emulation.go — never
// from adapter code behavior.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// emuCapture records the incoming upstream requests for assertions.
type emuCapture struct {
	bodies []string
	paths  []string
	auth   string
}

// newEmuUpstream serves one JSON body per request (in order) and records
// every incoming request.
func newEmuUpstream(t *testing.T, c *emuCapture, bodies ...string) *httptest.Server {
	t.Helper()
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.bodies = append(c.bodies, string(b))
		c.paths = append(c.paths, r.URL.Path)
		c.auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		body := "{}"
		if i < len(bodies) {
			body = bodies[i]
		}
		i++
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// 1. Search maps ServerToolConfig domain filters onto the documented
// include/exclude lists (F2) and returns web[] entries as IR search items.
func TestEmulator_SearchMapsDomainFiltersAndResults(t *testing.T) {
	var c emuCapture
	srv := newEmuUpstream(t, &c, `{"success":true,"data":{"web":[
		{"title":"Downloads - The Go Programming Language","url":"https://go.dev/dl/","description":"d","markdown":"m"}]}}`)
	emu := NewEmulator(srv.URL, "fc-emu-key", nil)

	st := ir.ServerToolConfig{
		AllowedDomains: []string{"example.com", "go.dev"},
		BlockedDomains: []string{"untrusted.example"},
	}
	items, err := emu.Search(context.Background(), "latest Go release", st)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	want := ir.WebSearchResultItem{
		URL:   "https://go.dev/dl/",
		Title: "Downloads - The Go Programming Language",
	}
	if items[0] != want {
		t.Errorf("items[0] = %+v, want %+v", items[0], want)
	}

	// Wire shape: query + domain filters on /v2/search.
	if c.paths[0] != "/v2/search" {
		t.Errorf("path = %q, want /v2/search", c.paths[0])
	}
	if c.auth != "Bearer fc-emu-key" {
		t.Errorf("Authorization = %q, want Bearer fc-emu-key", c.auth)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(c.bodies[0]), &body); err != nil {
		t.Fatalf("request body not JSON: %v", err)
	}
	if body["query"] != "latest Go release" {
		t.Errorf("query = %v, want latest Go release", body["query"])
	}
	if got, _ := body["includeDomains"].([]any); len(got) != 2 {
		t.Errorf("includeDomains = %v, want the allowed_domains list", body["includeDomains"])
	}
	if got, _ := body["excludeDomains"].([]any); len(got) != 1 {
		t.Errorf("excludeDomains = %v, want the blocked_domains list", body["excludeDomains"])
	}
}

// 2. Search without domain filters sends no include/exclude lists.
func TestEmulator_SearchWithoutFilters(t *testing.T) {
	var c emuCapture
	srv := newEmuUpstream(t, &c, `{"success":true,"data":{"web":[]}}`)
	emu := NewEmulator(srv.URL, "fc-emu-key", nil)

	if _, err := emu.Search(context.Background(), "q", ir.ServerToolConfig{}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(c.bodies[0]), &body); err != nil {
		t.Fatalf("request body not JSON: %v", err)
	}
	if _, present := body["includeDomains"]; present {
		t.Error("includeDomains must be absent without allowed_domains")
	}
	if _, present := body["excludeDomains"]; present {
		t.Error("excludeDomains must be absent without blocked_domains")
	}
}

// 3. Fetch maps a scrape (F3) with markdown format onto the normalized
// WebFetchContent payload (text source, text/plain media type).
func TestEmulator_FetchScrapesMarkdown(t *testing.T) {
	var c emuCapture
	srv := newEmuUpstream(t, &c, `{"success":true,"data":{
		"markdown":"# Downloads\n\nFeatured downloads.",
		"metadata":{"title":"Downloads - The Go Programming Language","url":"https://go.dev/dl/","statusCode":200}}}`)
	emu := NewEmulator(srv.URL, "fc-emu-key", nil)

	fc, err := emu.Fetch(context.Background(), "https://go.dev/dl/", ir.ServerToolConfig{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if fc == nil {
		t.Fatal("fetch content must not be nil")
	}
	if fc.URL != "https://go.dev/dl/" {
		t.Errorf("url = %q, want the requested url", fc.URL)
	}
	if fc.Title != "Downloads - The Go Programming Language" {
		t.Errorf("title = %q, want the scraped page title", fc.Title)
	}
	if fc.Source != "text" || fc.MediaType != "text/plain" {
		t.Errorf("source/media_type = %q/%q, want text/text/plain", fc.Source, fc.MediaType)
	}
	if fc.Data != "# Downloads\n\nFeatured downloads." {
		t.Errorf("data = %q, want the scraped markdown", fc.Data)
	}
	if fc.RetrievedAt == "" {
		t.Error("retrieved_at must be set (fetch timestamp)")
	}

	// Wire shape: url + markdown format on /v2/scrape.
	if c.paths[0] != "/v2/scrape" {
		t.Errorf("path = %q, want /v2/scrape", c.paths[0])
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(c.bodies[0]), &body); err != nil {
		t.Fatalf("request body not JSON: %v", err)
	}
	if body["url"] != "https://go.dev/dl/" {
		t.Errorf("url = %v", body["url"])
	}
	formats, _ := body["formats"].([]any)
	if len(formats) != 1 || formats[0] != "markdown" {
		t.Errorf("formats = %v, want [markdown]", body["formats"])
	}
}

// 4. CitationsEnabled rides through from the definition knobs.
func TestEmulator_FetchCarriesCitationsFlag(t *testing.T) {
	var c emuCapture
	srv := newEmuUpstream(t, &c, `{"success":true,"data":{"markdown":"m","metadata":{"title":"t","url":"u"}}}`)
	emu := NewEmulator(srv.URL, "fc-emu-key", nil)

	fc, err := emu.Fetch(context.Background(), "https://go.dev/", ir.ServerToolConfig{CitationsEnabled: true})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !fc.CitationsEnabled {
		t.Error("citations_enabled must ride through from the definition knobs")
	}
}

// 5. Upstream API errors surface as errors (the loop maps them to the
// documented "unavailable" error code), never as empty results.
func TestEmulator_UpstreamErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"success":false,"error":"Plan rate limit reached"}`))
	}))
	t.Cleanup(srv.Close)
	emu := NewEmulator(srv.URL, "fc-emu-key", nil)

	if _, err := emu.Search(context.Background(), "q", ir.ServerToolConfig{}); err == nil {
		t.Error("search must surface upstream API errors")
	}
	if _, err := emu.Fetch(context.Background(), "https://go.dev/", ir.ServerToolConfig{}); err == nil {
		t.Error("fetch must surface upstream API errors")
	}
}

// 6. A success:false body is an error too (documented contract, F5).
func TestEmulator_SuccessFalseIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":false,"error":"something"}`))
	}))
	t.Cleanup(srv.Close)
	emu := NewEmulator(srv.URL, "fc-emu-key", nil)

	if _, err := emu.Search(context.Background(), "q", ir.ServerToolConfig{}); err == nil {
		t.Error("success:false must surface as an error")
	}
}

// 7. The golden fixtures must still decode through the adapter end-to-end.
func TestEmulator_GoldenFixturesDecode(t *testing.T) {
	var c emuCapture
	srv := newEmuUpstream(t, &c,
		`{"success":true,"data":{"web":[
			{"title":"Downloads - The Go Programming Language","url":"https://go.dev/dl/","description":"Download the latest Go release binaries.","markdown":"# Downloads"},
			{"title":"Go Release History","url":"https://go.dev/doc/devel/release","description":"Release history."}]}}`,
		`{"success":true,"data":{"markdown":"# Downloads","metadata":{"title":"Downloads - The Go Programming Language","url":"https://go.dev/dl/","statusCode":200}}}`)
	emu := NewEmulator(srv.URL, "fc-emu-key", nil)

	items, err := emu.Search(context.Background(), "go", ir.ServerToolConfig{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("golden search items = %d, want 2", len(items))
	}
	if items[0].URL != "https://go.dev/dl/" || items[0].Title != "Downloads - The Go Programming Language" {
		t.Errorf("items[0] = %+v", items[0])
	}

	fc, err := emu.Fetch(context.Background(), "https://go.dev/dl/", ir.ServerToolConfig{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if fc.Title != "Downloads - The Go Programming Language" || fc.Data != "# Downloads" {
		t.Errorf("golden fetch = %+v", fc)
	}
}

// 8. Empty search results are a valid, non-error answer.
func TestEmulator_EmptySearchIsNotError(t *testing.T) {
	var c emuCapture
	srv := newEmuUpstream(t, &c, `{"success":true,"data":{"web":[]}}`)
	emu := NewEmulator(srv.URL, "fc-emu-key", nil)

	items, err := emu.Search(context.Background(), "obscure query", ir.ServerToolConfig{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("items = %d, want 0", len(items))
	}
}

// compile-time interface proof: Emulator satisfies routing.Emulator.
var _ interface {
	Search(ctx context.Context, query string, st ir.ServerToolConfig) ([]ir.WebSearchResultItem, error)
	Fetch(ctx context.Context, url string, st ir.ServerToolConfig) (*ir.WebFetchContent, error)
} = (*Emulator)(nil)
