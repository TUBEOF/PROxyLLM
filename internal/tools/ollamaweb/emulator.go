package ollamaweb

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tubeof/proxyllm/internal/httpguard"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// ErrDomainFiltersUnsupported is returned instead of answering a search whose
// definition carries domain filters. Ollama's /api/web_search accepts only a
// query, so returning unfiltered results would silently break the constraint
// the client asked for. Failing lets the emulation pool walk on to a backend
// that can honour it (Firecrawl), and — if none can — the client sees the
// documented error rather than a quietly wrong answer.
var ErrDomainFiltersUnsupported = errors.New("ollamaweb: domain filters are not supported by this backend")

// Emulator adapts the Ollama web-tool API to routing.Emulator. A nil
// *Emulator must not be constructed by callers: when no key is configured the
// account row still constructs and fails at call time with the documented
// upstream error — the same honest-state contract as the Firecrawl adapter.
type Emulator struct {
	client *Client
}

// emulatorClient is the HTTP client shared by every emulator account, for the
// same reason as the Firecrawl adapter's: accounts are built per provider row,
// and a private transport per account would mean a separate connection pool
// per account and no idle-connection reuse across a failover.
//
// The 20 s client timeout sits deliberately below the executor's 30 s
// per-invocation emulation budget: a stalled account has to give up in time
// for the next account to get a window inside that same budget.
var emulatorClient = sync.OnceValue(func() *http.Client {
	return httpguard.NewClient(20 * time.Second)
})

// NewEmulator builds the adapter. An empty baseURL delegates to the client
// default; a nil httpClient selects the shared emulator client.
func NewEmulator(baseURL, apiKey string, httpClient *http.Client) *Emulator {
	if httpClient == nil {
		httpClient = emulatorClient()
	}
	return &Emulator{client: New(baseURL, apiKey, httpClient)}
}

// Search answers a web_search invocation. The service's per-result `content`
// snippet is DROPPED: ir.WebSearchResultItem carries URL/Title/
// EncryptedContent/PageAge and the Anthropic `web_search_result` block has no
// plain-text field either, so there is nowhere on the wire to put it. Adding
// one would be a wire change with a fixture refresh, not a backend concern.
func (e *Emulator) Search(ctx context.Context, query string, st ir.ServerToolConfig) ([]ir.WebSearchResultItem, error) {
	if len(st.AllowedDomains) > 0 || len(st.BlockedDomains) > 0 {
		return nil, ErrDomainFiltersUnsupported
	}
	resp, err := e.client.Search(ctx, SearchRequest{Query: query})
	if err != nil {
		return nil, err
	}
	items := make([]ir.WebSearchResultItem, 0, len(resp.Results))
	for _, r := range resp.Results {
		items = append(items, ir.WebSearchResultItem{URL: r.URL, Title: r.Title})
	}
	return items, nil
}

// Fetch answers a web_fetch invocation: the service's extracted article text
// normalized into the WebFetchContent payload — text source with text/plain
// media type, mirroring the Anthropic document-block semantics, and the
// retrieval time stamped now.
//
// A page the service could not retrieve comes back with empty Content and is
// still returned non-nil: the emulation layer decides what an empty page means
// to the client (it maps it to the documented url_not_accessible error) and
// the pool's account walk must stay free to ask the next account instead of
// failing the whole call here.
func (e *Emulator) Fetch(ctx context.Context, url string, st ir.ServerToolConfig) (*ir.WebFetchContent, error) {
	resp, err := e.client.Fetch(ctx, FetchRequest{URL: url})
	if err != nil {
		return nil, err
	}
	return &ir.WebFetchContent{
		URL:              url,
		Title:            resp.Title,
		Source:           "text",
		MediaType:        "text/plain",
		Data:             strings.TrimSpace(resp.Content),
		CitationsEnabled: st.CitationsEnabled,
		RetrievedAt:      time.Now().UTC().Format(time.RFC3339),
	}, nil
}
