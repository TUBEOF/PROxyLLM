package routing

// WP8 fuzz coverage for the emulated web-tool loop's untrusted-input
// boundaries. The loop is in-package (same reasoning as pool_internal_test.go):
// the boundaries under test — answerEmulated's per-call path and the
// domainAllowed gate — are unexported.
//
// What counts as untrusted here:
//
//   - the model's tool-call input (call.Input is arbitrary JSON bytes, not
//     necessarily the input schema's shape),
//   - the web-tool backends' answers (Firecrawl /v2/search + /v2/scrape,
//     Ollama /api/web_search + /api/web_fetch — free-form strings that get
//     re-embedded into the model-facing result payload and the trace),
//   - the client-declared tool knobs the budget arithmetic multiplies
//     (max_uses slots, max_content_tokens × 4 bytes).
//
// The seeds come from the CONTRACT shapes, never from this implementation's
// output. Sources (also the golden fixtures' sources, per their .meta.json):
//
//   - tool-call input shapes and server-tool knobs/error codes:
//     https://docs.anthropic.com/en/docs/agents-and-tools/tool-use/web-search-tool
//     and https://docs.anthropic.com/en/docs/agents-and-tools/tool-use/web-fetch-tool
//     (testdata/golden/anthropic/server-tool-web-*.json)
//   - backend answer shapes:
//     https://docs.firecrawl.dev/api-reference/endpoint/search and
//     https://docs.firecrawl.dev/api-reference/endpoint/scrape
//     (testdata/golden/firecrawl/{search,scrape}-response.json),
//     https://docs.ollama.com/capabilities/web-search
//
// Not targeted here: the round-trip LOOP's growth bounds (maxEmulationRounds
// and EmulationTotalBudget) — both are pinned by the emulation budget unit
// tests, and driving them would need the full provider chain. The fuzz
// bodies themselves are bounded to a fixed number of invocations, and every
// call's allocations stay proportional to the fuzz input's size.

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// fuzzEmulator is a scriptable Emulator: it stands in for whatever a
// Firecrawl or Ollama web-tool account answered, with the answer taken
// verbatim from the fuzz input. Every Search/Fetch call builds a FRESH
// answer object — answerEmulated may mutate the payload it accepted
// (the max_content_tokens truncation writes into fc.Data), and a real
// backend refetches rather than returning the same struct twice.
type fuzzEmulator struct {
	snippet  string // web_search backend content (Firecrawl data.web[].description / Ollama results[].content)
	pageData string // web_fetch backend page body (Firecrawl data.markdown / Ollama fetch content)
}

func (m *fuzzEmulator) Search(context.Context, string, ir.ServerToolConfig) ([]ir.WebSearchResultItem, error) {
	return []ir.WebSearchResultItem{{
		URL:     "https://go.dev/ref/spec",
		Title:   "The Go Programming Language Specification",
		Snippet: m.snippet,
	}}, nil
}

func (m *fuzzEmulator) Fetch(_ context.Context, fetchURL string, _ ir.ServerToolConfig) (*ir.WebFetchContent, error) {
	return &ir.WebFetchContent{URL: fetchURL, Data: m.pageData}, nil
}

// documentedErrorCode reports whether a code is one of the emulation loop's
// documented server-tool error codes — the set the loop itself may emit.
// (Provider-native codes like too_many_requests or query_too_long belong to
// upstreams, never to this loop's results.)
func documentedErrorCode(code string) bool {
	switch code {
	case errCodeMaxUsesExceeded, errCodeInvalidToolInput, errCodeURLNotInPriorCtx,
		errCodeURLNotAllowed, errCodeURLNotAccessible, errCodeEmulatorUnavailable:
		return true
	}
	return false
}

// fuzzResultText reads the model-facing text of a tool result (the loop
// writes exactly one text part).
func fuzzResultText(tr ir.ToolResultPart) string {
	if len(tr.Content) == 1 {
		if tp, ok := tr.Content[0].(ir.TextPart); ok {
			return tp.Text
		}
	}
	return ""
}

// FuzzEmulatedCallAnswer drives answerEmulated — the loop's per-call path —
// with adversarial inputs on all three boundaries: the call's raw input
// JSON, the backend's answer strings, and the tool knobs. Per invocation the
// documented contracts are: a refusal is always the {"error_code": ...} shape
// with a code from the documented set (never upstream error text), and a
// successful web_fetch carries the page bounded by max_content_tokens (4
// chars/token) or untouched when the truncation guard does not fire. Across
// invocations the max_uses slot gate must hold: only successful invocations
// consume a slot, and past max_uses every further call is refused
// (max_uses_exceeded) — a crafted response can neither exceed the budget nor
// skip the gate.
func FuzzEmulatedCallAnswer(f *testing.F) {
	seeds := []struct {
		input                     string // call.Input (arbitrary JSON)
		search                    bool   // true = web_search family, false = web_fetch
		maxUses, maxContentTokens int
		snippet, page, prior      string // prior = user text the fetch gate scans
	}{
		// Documented input shapes: query for web_search, url for web_fetch.
		{input: `{"query":"golang regexp"}`, search: true, maxUses: 8,
			snippet: "The regexp package implements regular expression search."},
		{input: `{"url":"https://go.dev/ref/spec"}`, maxUses: 8,
			page:  "package main — the specification's page text, as retrieved.",
			prior: "read https://go.dev/ref/spec before answering"},
		// A fetch of a url the model invented must be refused by the
		// prior-context gate, without consuming a slot.
		{input: `{"url":"https://go.dev/ref/spec"}`, maxUses: 8, prior: "nothing here"},
		// A max_uses boundary: 2 slots only.
		{input: `{"query":"go"}`, search: true, maxUses: 2, snippet: "go.dev results."},
		// A 200-answered but unservable page (Firecrawl scrape of a 404):
		// empty content is url_not_accessible, not a success.
		{input: `{"url":"https://go.dev/ref/spec"}`, maxUses: 8,
			prior: "see https://go.dev/ref/spec"},
		// max_content_tokens truncates (4 chars/token); a value near MaxInt
		// must leave the page untouched — that ordering is the guard against
		// the ×4 overflow.
		{input: `{"url":"https://go.dev/ref/spec"}`, maxUses: 8, maxContentTokens: 8,
			page: "0123456789 héllo wörld ünïcödé tail…", prior: "see https://go.dev/ref/spec"},
		{input: `{"url":"https://go.dev/ref/spec"}`, maxUses: 8, maxContentTokens: 1 << 62,
			page: "short page", prior: "see https://go.dev/ref/spec"},
		// Malformed input shapes: refused with invalid_tool_input, no slot.
		{input: `null`, search: true, maxUses: 8},
		{input: `[1,2]`, search: true, maxUses: 8},
		{input: `{"query":123}`, search: true, maxUses: 8},
		{input: `{"query":"   "}`, search: true, maxUses: 8},
		{input: `{"url":""}`, maxUses: 8},
		{input: `{"url":"http://169.254.169.254/latest"}`, maxUses: 8, prior: "nothing here"},
	}
	for _, s := range seeds {
		f.Add([]byte(s.input), s.search, s.maxUses, s.maxContentTokens,
			s.snippet, s.page, s.prior)
	}
	f.Fuzz(func(t *testing.T, input []byte, search bool, maxUses, maxContentTokens int,
		snippet, page, prior string) {
		// snippet, page and prior model POST-decode strings: a backend's
		// body is decoded through encoding/json (Firecrawl SearchResponse /
		// ScrapeResponse, Ollama SearchResponse / FetchResponse) whose string
		// fields are always valid UTF-8 — invalid bytes were replaced at
		// that boundary. `input` alone is the raw-bytes boundary.
		snippet = strings.ToValidUTF8(snippet, "�")
		page = strings.ToValidUTF8(page, "�")
		prior = strings.ToValidUTF8(prior, "�")
		family := "web_search"
		if !search {
			family = "web_fetch"
		}
		emu := &fuzzEmulator{snippet: snippet, pageData: page}
		e := NewExecutor(ExecutorConfig{
			Emulator: emu,
			now:      func() time.Time { return time.Unix(0, 0) },
		})
		call := ir.ToolCallPart{ID: "toolu_fuzz", Name: family, Input: input}
		st := ir.ServerToolConfig{MaxUses: maxUses, MaxContentTokens: maxContentTokens}
		req := ir.Request{Messages: []ir.Message{{Role: ir.RoleUser,
			Parts: []ir.Part{ir.TextPart{Text: prior}}}}}
		trace := &Trace{}
		uses := map[string]int{}

		// Six invocations share one uses map exactly as the round-trip loop
		// does across rounds: past a positive max_uses everything further
		// must be refused without consuming a slot.
		successes := 0
		for i := 0; i < 6; i++ {
			_, res, tr := e.answerEmulated(context.Background(), call, st, req, uses, trace)
			switch {
			case res.IsError:
				text := fuzzResultText(tr)
				var wire map[string]string
				if err := json.Unmarshal([]byte(text), &wire); err != nil {
					t.Fatalf("round %d: error result text %q is not a JSON object: %v", i, text, err)
				}
				if wire["error_code"] != res.ErrorCode {
					t.Fatalf("round %d: text names %q, result part carries %q",
						i, wire["error_code"], res.ErrorCode)
				}
				if !documentedErrorCode(res.ErrorCode) {
					t.Fatalf("round %d: undocumented error code %q", i, res.ErrorCode)
				}
				if !json.Valid([]byte(text)) {
					t.Fatalf("round %d: error result text %q is not valid JSON", i, text)
				}
			default:
				successes++
				fc := res.Fetch
				if family == "web_fetch" && fc != nil {
					if !utf8.ValidString(fc.Data) {
						t.Fatalf("round %d: fetch data is not valid UTF-8", i)
					}
					// The truncation guard's own ordering: with a positive
					// max_content_tokens below the page length the page is
					// bounded (the … mark may add its 3 bytes); with any
					// other value — including the near-MaxInt overflow
					// corner — the page must ride through untouched.
					if maxContentTokens > 0 && maxContentTokens < len(page) {
						if len(fc.Data) > 4*maxContentTokens+3 {
							t.Fatalf("round %d: fetch data %d bytes exceeds the content budget (%d tokens × 4)",
								i, len(fc.Data), maxContentTokens)
						}
					} else if fc.Data != page {
						t.Fatalf("round %d: fetch data changed although the truncation guard did not fire", i)
					}
				}
			}
		}
		if used := uses[family]; used != successes {
			t.Fatalf("uses[%s] = %d, successes = %d — a failed or refused call must not consume a slot",
				family, used, successes)
		}
		if maxUses > 0 && successes > maxUses {
			t.Fatalf("slot gate exceeded: %d successes, max_uses %d", successes, maxUses)
		}
	})
}

// FuzzWebFetchDomainGate drives the web_fetch domain filters with arbitrary
// urls and filter lists — the SSRF gate's verdict function. Invariants: an
// accepted url must parse to a lowercase http(s) url with a non-empty host;
// any listed blocked (sub)domain must never pass; a non-empty allowed list is
// an allowlist; every refusal carries exactly one of the two documented
// codes (url_not_allowed, or invalid_tool_input when the url does not parse
// to http(s)); and a blocked domain wins over an allowed one.
func FuzzWebFetchDomainGate(f *testing.F) {
	seeds := []struct{ rawURL, allowed, blocked string }{
		{rawURL: "https://go.dev/ref/spec"},
		{rawURL: "https://docs.example.com/docs/page", allowed: "example.com"},
		{rawURL: "https://docs.example.com/docs/page", allowed: "go.dev"},
		{rawURL: "https://api.example.com/v2/page", allowed: "example.com,api.example.com"},
		{rawURL: "https://api.example.com/v2/page", blocked: "api.example.com"},
		// A blocked domain dominates allowed (the doc's rule for
		// allowed_domains ∪ blocked_domains).
		{rawURL: "https://tracker.example.com/a", allowed: "example.com", blocked: "tracker.example.com"},
		// The suffix rule is host suffix ".domain" — never a bare string
		// suffix (example.com.evil.net does not inherit example.com).
		{rawURL: "https://example.com.evil.net/", allowed: "evil.net"},
		{rawURL: "https://notexample.com/", blocked: "example.com", allowed: "notexample.com"},
		// Case, userinfo and ports: the filters read the lowercased hostname.
		{rawURL: "HTTP://EVIL.COM:8080/page", blocked: "evil.com"},
		{rawURL: "https://user:pw@example.com/page", allowed: "example.com"}, //nolint:gosec // G101: fixture url in a fuzz seed, not a credential
		// Non-http(s) or unparseable: invalid input, not a domain verdict.
		{rawURL: "file:///etc/passwd"},
		{rawURL: "example.com/page"},
		{rawURL: "https://exa%2Fmple.com/"},
		{rawURL: "https://[::1]:80/"},
		{rawURL: "https://münchen.de/"},
		{rawURL: "https://example.com\\@go.dev/"},
	}
	for _, s := range seeds {
		f.Add(s.rawURL, s.allowed, s.blocked)
	}
	// Seeds from the knob contracts: allowed_domains/blocked_domains on the
	// web-fetch tool reference (see the fixture sources above) and the
	// include/excludeDomains search knobs.
	f.Fuzz(func(t *testing.T, rawURL, allowedCSV, blockedCSV string) {
		st := ir.ServerToolConfig{
			AllowedDomains: strings.Split(allowedCSV, ","),
			BlockedDomains: strings.Split(blockedCSV, ","),
		}
		ok, code := domainAllowed(rawURL, st)
		switch {
		case ok && code != "":
			t.Fatalf("accepted %q with code %q; an accepted url carries no error code", rawURL, code)
		case !ok && code != errCodeInvalidToolInput && code != errCodeURLNotAllowed:
			t.Fatalf("rejected %q with undocumented code %q", rawURL, code)
		}
		u, err := url.Parse(rawURL)
		parseOK := err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
		if !parseOK && ok {
			t.Fatalf("accepted %q: does not parse to a http(s) url with a host", rawURL)
		}
		if !parseOK {
			return
		}
		host := strings.ToLower(u.Hostname())
		for _, d := range st.BlockedDomains {
			d = strings.ToLower(strings.TrimSpace(d))
			if d != "" && (host == d || strings.HasSuffix(host, "."+d)) && ok {
				t.Fatalf("accepted host %q although listed blocked by %q", host, d)
			}
		}
		if len(st.AllowedDomains) > 0 && ok {
			matched := false
			for _, d := range st.AllowedDomains {
				d = strings.ToLower(strings.TrimSpace(d))
				if d != "" && (host == d || strings.HasSuffix(host, "."+d)) {
					matched = true
					break
				}
			}
			if !matched {
				t.Fatalf("accepted host %q although not covered by allowlist %v", host, st.AllowedDomains)
			}
		}
	})
}
