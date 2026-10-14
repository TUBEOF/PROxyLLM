// Emulator adapter: the routing.Emulator implementation on top of the v2
// client. The routing engine declares the
// interface (dependency inversion, internal/routing/emulation.go); this
// package is the production wiring. The adapter does NOT import routing —
// the interface proof lives in the routing-side wiring test, so the tool
// backend stays decoupled from the engine.
package firecrawl

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tubeof/proxyllm/internal/httpguard"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// Emulator executes emulated web tools via the Firecrawl v2 client. A nil
// *Emulator must not be constructed by callers: when no API key is
// configured, assembly leaves routing's Emulator unset and the emulation
// loop degrades to the documented "unavailable" error results.
type Emulator struct {
	client *Client
}

// emulatorClient is the HTTP client shared by every emulator account.
//
// Accounts are built per provider row; cloning a fresh transport per account
// would make one pool walk dial a different connection
// pool for each account, and an idle connection would never be reused across a
// failover. One client, one pool, which is what makes the per-host idle settings
// in httpguard mean anything here.
//
// The 20 s client timeout is deliberately below the executor's per-invocation
// emulation budget (30 s): a stalled account has to give up in time for the next
// account to get a window inside that same budget.
var emulatorClient = sync.OnceValue(func() *http.Client {
	return httpguard.NewClient(20 * time.Second)
})

// NewEmulator builds the adapter. An empty baseURL delegates to the client
// default; a nil httpClient selects the shared emulator client (see
// emulatorClient) rather than a private transport per account.
func NewEmulator(baseURL, apiKey string, httpClient *http.Client) *Emulator {
	if httpClient == nil {
		httpClient = emulatorClient()
	}
	return &Emulator{client: New(baseURL, apiKey, httpClient)}
}

// Search answers a web_search invocation: the definition's domain filters
// map onto the documented include/exclude parameter lists, and every
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

// Fetch answers a web_fetch invocation: a markdown scrape normalized
// into the WebFetchContent payload — text source with text/plain media
// type, mirroring the Anthropic document-block semantics, and the
// scrape timestamp as retrieval time.
//
// A page Firecrawl could not serve comes back EMPTY, not as an error: the
// documented 200 body carries the page's own status and error note, so a 404 or
// a failed extraction is this account having nothing to give — and the caller's
// account walk must be free to ask the next one instead of failing the whole
// call. The empty payload is indistinguishable from a page with no text, which
// is the honest reading: the model is told nothing was retrieved either way.
func (e *Emulator) Fetch(ctx context.Context, url string, st ir.ServerToolConfig) (*ir.WebFetchContent, error) {
	resp, err := e.client.Scrape(ctx, ScrapeRequest{
		URL:     url,
		Formats: []string{"markdown"},
	})
	if err != nil {
		return nil, err
	}
	fc := &ir.WebFetchContent{
		URL:              url,
		Title:            resp.Data.Metadata.Title,
		Source:           "text",
		MediaType:        "text/plain",
		CitationsEnabled: st.CitationsEnabled,
		RetrievedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	if pageUsable(resp) {
		fc.Data = resp.Data.Markdown
	}
	return fc, nil
}

// pageUsable reports whether a scrape actually produced a document. Firecrawl
// answers HTTP 200 even for a page it could not fetch, carrying the target's
// own status in metadata.statusCode (and an error note beside it), so the
// status code — not the transport — is what says whether there is content. A
// zero status is treated as usable: some responses omit it, and the markdown
// check already covers the empty case.
func pageUsable(resp *ScrapeResponse) bool {
	if strings.TrimSpace(resp.Data.Markdown) == "" {
		return false
	}
	sc := resp.Data.Metadata.StatusCode
	return sc == 0 || (sc >= 200 && sc < 300)
}
