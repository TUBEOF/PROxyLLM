// Emulator adapter: the routing.Emulator implementation on top of the v2
// client (plan §6.2 Firecrawl fallback). The routing engine declares the
// interface (dependency inversion, internal/routing/emulation.go); this
// package is the production wiring. The adapter does NOT import routing —
// the interface proof lives in the routing-side wiring test, so the tool
// backend stays decoupled from the engine.
package firecrawl

import (
	"context"
	"net/http"
	"time"

	"github.com/proxyllm/proxyllm/internal/translate/ir"
)

// Emulator executes emulated web tools via the Firecrawl v2 client. A nil
// *Emulator must not be constructed by callers: when no API key is
// configured, assembly leaves routing's Emulator unset and the emulation
// loop degrades to the documented "unavailable" error results.
type Emulator struct {
	client *Client
}

// NewEmulator builds the adapter. An empty baseURL or nil httpClient
// delegates to the client defaults.
func NewEmulator(baseURL, apiKey string, httpClient *http.Client) *Emulator {
	return &Emulator{client: New(baseURL, apiKey, httpClient)}
}

// Search answers a web_search invocation: the definition's domain filters
// map onto the documented include/exclude parameter lists (F2), and every
// returned web[] entry becomes an IR search item. Firecrawl returns titles
// and URLs — not Anthropic's provider-encrypted page content — so
// EncryptedContent stays empty: the emulation layer never fabricates
// opaque provider data.
func (e *Emulator) Search(ctx context.Context, query string, st ir.ServerToolConfig) ([]ir.WebSearchResultItem, error) {
	resp, err := e.client.Search(ctx, SearchRequest{
		Query:          query,
		IncludeDomains: st.AllowedDomains,
		ExcludeDomains: st.BlockedDomains,
	})
	if err != nil {
		return nil, err
	}
	items := make([]ir.WebSearchResultItem, 0, len(resp.Data.Web))
	for _, w := range resp.Data.Web {
		items = append(items, ir.WebSearchResultItem{
			URL:   w.URL,
			Title: w.Title,
		})
	}
	return items, nil
}

// Fetch answers a web_fetch invocation: a markdown scrape (F3) normalized
// into the WebFetchContent payload — text source with text/plain media
// type, mirroring the Anthropic document-block semantics (A3), and the
// scrape timestamp as retrieval time.
func (e *Emulator) Fetch(ctx context.Context, url string, st ir.ServerToolConfig) (*ir.WebFetchContent, error) {
	resp, err := e.client.Scrape(ctx, ScrapeRequest{
		URL:     url,
		Formats: []string{"markdown"},
	})
	if err != nil {
		return nil, err
	}
	return &ir.WebFetchContent{
		URL:              url,
		Title:            resp.Data.Metadata.Title,
		Source:           "text",
		MediaType:        "text/plain",
		Data:             resp.Data.Markdown,
		CitationsEnabled: st.CitationsEnabled,
		RetrievedAt:      time.Now().UTC().Format(time.RFC3339),
	}, nil
}
