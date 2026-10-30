package ollamaweb_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tubeof/proxyllm/internal/tools/ollamaweb"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// irServerToolConfig is the zero server-tool definition: no domain filters, no
// citation request — the shape a plain web_search/web_fetch call carries.
func irServerToolConfig() ir.ServerToolConfig { return ir.ServerToolConfig{} }

// Wire facts these tests pin come from the official docs, read 2026-10-11
// (https://docs.ollama.com/capabilities/web-search):
//   - POST /api/web_search  {query, max_results} →
//     {results:[{title,url,content}]}
//   - POST /api/web_fetch   {url}                → {title,content,links[]}
//   - Authorization: Bearer <OLLAMA_API_KEY>
//   - no domain filters exist on the search endpoint

// searchServer answers /api/web_search with the documented body and records
// the request it saw.
func searchServer(t *testing.T, body string, status int) (*httptest.Server, *http.Request, *[]byte) {
	t.Helper()
	var seen http.Request
	var seenBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = *r
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		seenBody = buf
		if r.URL.Path != "/api/web_search" {
			t.Errorf("path = %q, want /api/web_search", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen, &seenBody
}

func TestEmulator_SearchMapsResultsAndSendsTheBearerKey(t *testing.T) {
	srv, seen, _ := searchServer(t, `{"results":[
		{"title":"Ollama","url":"https://ollama.com/","content":"Cloud models are now available..."},
		{"title":"Docs","url":"https://docs.ollama.com/","content":"Guides"}
	]}`, http.StatusOK)

	emu := ollamaweb.NewEmulator(srv.URL, "test-key", srv.Client())
	items, err := emu.Search(context.Background(), "what is ollama", irServerToolConfig())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if items[0].URL != "https://ollama.com/" || items[0].Title != "Ollama" {
		t.Errorf("first item = %+v", items[0])
	}
	// The snippet rides its own field (model-facing only); never folded
	// into the title.
	if items[0].Snippet != "Cloud models are now available..." {
		t.Errorf("Snippet = %q, want the service's content", items[0].Snippet)
	}
	if items[0].Title == items[0].Snippet {
		t.Error("the content snippet must not be folded into the title")
	}
	if got := seen.Header.Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", got)
	}
}

func TestEmulator_SearchRefusesDomainFilters(t *testing.T) {
	// The endpoint has no domain parameters, so answering would silently
	// return unfiltered results for a request that asked for a filter. The
	// walk must be free to try a backend that can honour it.
	srv, _, _ := searchServer(t, `{"results":[]}`, http.StatusOK)
	emu := ollamaweb.NewEmulator(srv.URL, "test-key", srv.Client())

	st := irServerToolConfig()
	st.AllowedDomains = []string{"example.com"}
	if _, err := emu.Search(context.Background(), "q", st); !errors.Is(err, ollamaweb.ErrDomainFiltersUnsupported) {
		t.Errorf("allowed_domains: err = %v, want ErrDomainFiltersUnsupported", err)
	}

	st = irServerToolConfig()
	st.BlockedDomains = []string{"private.example.com"}
	if _, err := emu.Search(context.Background(), "q", st); !errors.Is(err, ollamaweb.ErrDomainFiltersUnsupported) {
		t.Errorf("blocked_domains: err = %v, want ErrDomainFiltersUnsupported", err)
	}
}

func TestEmulator_FetchMapsThePage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/web_fetch" {
			t.Errorf("path = %q, want /api/web_fetch", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"title":"Article Title","content":"Full text of the article.",
			"links":["https://example.com/"]}`))
	}))
	defer srv.Close()

	st := irServerToolConfig()
	st.CitationsEnabled = true
	emu := ollamaweb.NewEmulator(srv.URL, "test-key", srv.Client())
	fc, err := emu.Fetch(context.Background(), "https://example.com/article", st)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if fc.URL != "https://example.com/article" || fc.Title != "Article Title" {
		t.Errorf("url/title = %q/%q", fc.URL, fc.Title)
	}
	// The document-block semantics the Anthropic wire shape expects.
	if fc.Source != "text" || fc.MediaType != "text/plain" {
		t.Errorf("source/media = %q/%q, want text/text/plain", fc.Source, fc.MediaType)
	}
	if fc.Data != "Full text of the article." {
		t.Errorf("data = %q", fc.Data)
	}
	if !fc.CitationsEnabled {
		t.Error("citations flag lost")
	}
	if fc.RetrievedAt == "" {
		t.Error("retrieved_at must be stamped")
	}
}

func TestEmulator_FetchOfAnUnservablePageIsAnEmptyAnswer(t *testing.T) {
	// A page the service could not retrieve is an EMPTY answer, not a
	// transport failure: the pool must be free to ask the next account, and
	// the emulation layer maps the empty page to the documented error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"title":"","content":"","links":[]}`))
	}))
	defer srv.Close()

	emu := ollamaweb.NewEmulator(srv.URL, "test-key", srv.Client())
	fc, err := emu.Fetch(context.Background(), "https://example.com/gone", irServerToolConfig())
	if err != nil {
		t.Fatalf("an unservable page must not be an error here: %v", err)
	}
	if fc == nil {
		t.Fatal("the page must ride back non-nil so the walk can judge it")
	}
	if fc.Data != "" {
		t.Errorf("data = %q, want empty", fc.Data)
	}
}

func TestEmulator_Non2xxIsAnAPIErrorAndTransportIsNot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer srv.Close()

	emu := ollamaweb.NewEmulator(srv.URL, "bad", srv.Client())
	_, err := emu.Fetch(context.Background(), "https://example.com/", irServerToolConfig())

	var apiErr *ollamaweb.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v (%T), want *APIError", err, err)
	}
	if apiErr.HTTPStatus != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", apiErr.HTTPStatus)
	}
	if apiErr.ErrorMsg != "invalid api key" {
		t.Errorf("message = %q, want the service's own text", apiErr.ErrorMsg)
	}
}

// TestEmulator_EmptyBaseURLSelectsTheHostedEndpoint pins the default the
// account row relies on when the operator leaves base_url blank.
func TestEmulator_EmptyBaseURLSelectsTheHostedEndpoint(t *testing.T) {
	c := ollamaweb.New("", "k", nil)
	if c.BaseURL() != ollamaweb.DefaultBaseURL {
		t.Errorf("base URL = %q, want %q", c.BaseURL(), ollamaweb.DefaultBaseURL)
	}
	if ollamaweb.DefaultBaseURL != "https://ollama.com" {
		t.Errorf("DefaultBaseURL = %q — the documented host changed", ollamaweb.DefaultBaseURL)
	}
}

// TestEmulator_MaxResultsIsOmittedWhenUnset — the emulation has no
// result-count knob, so the service's documented default (5, capped at 10)
// must apply rather than an invented value.
func TestEmulator_MaxResultsIsOmittedWhenUnset(t *testing.T) {
	srv, _, body := searchServer(t, `{"results":[]}`, http.StatusOK)
	emu := ollamaweb.NewEmulator(srv.URL, "k", srv.Client())
	if _, err := emu.Search(context.Background(), "q", irServerToolConfig()); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := string(*body); got != `{"query":"q"}` {
		t.Errorf("request body = %s, want only the query", got)
	}
}
