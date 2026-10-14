package routing_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// Executor tool-call round-trip tests for the Firecrawl emulation:
// when a request carries emulated web tools (Emulated marker from the
// emulate-firecrawl compat policy) and the model answers with a tool call
// against one of them, the executor loops: Firecrawl → tool result →
// re-dispatch, until the model answers without a tool call (or the
// max-uses budget is exhausted → protocol-conform max_uses_exceeded).
//
// Wire facts the synthesized results must honor (verified live 2026-09-12,
// golden fixtures server-tool-web-*.json):
//   - web_search result content: ARRAY of web_search_result blocks
//   - web_fetch result content: single web_fetch_result object with the
//     title/citations on the nested document block
//   - usage counters: server_tool_use.web_search_requests /
//     web_fetch_requests increment per invocation (honest accounting)
//   - web_fetch may only fetch URLs that appeared in prior conversation
//     context (user messages, client tool results, prior search/fetch
//     results) — never model output
//   - max_uses exceeded → IsError result with error_code
//     "max_uses_exceeded", not an upstream error

// fakeEmulator scripts the Firecrawl answers for the round-trip tests:
// behavior is scripted up front.
type fakeEmulator struct {
	searchAnswers []string // JSON array of web results, one per search call
	fetchAnswers  []string // JSON page content, one per fetch call

	searchCalls, fetchCalls int
}

func (f *fakeEmulator) Search(ctx context.Context, query string, st ir.ServerToolConfig) ([]ir.WebSearchResultItem, error) {
	i := f.searchCalls
	f.searchCalls++
	if i < len(f.searchAnswers) {
		var items []ir.WebSearchResultItem
		if err := json.Unmarshal([]byte(f.searchAnswers[i]), &items); err != nil {
			return nil, err
		}
		return items, nil
	}
	return nil, nil
}

func (f *fakeEmulator) Fetch(ctx context.Context, url string, st ir.ServerToolConfig) (*ir.WebFetchContent, error) {
	i := f.fetchCalls
	f.fetchCalls++
	if i < len(f.fetchAnswers) {
		var fc ir.WebFetchContent
		if err := json.Unmarshal([]byte(f.fetchAnswers[i]), &fc); err != nil {
			return nil, err
		}
		return &fc, nil
	}
	return nil, nil
}

// emulProvider answers the first Complete with a web_search tool call and
// the second with a plain answer (the model "used" the tool).
type emulProvider struct {
	calls   int
	lastReq ir.Request
	// streamCalls counts Stream dispatches (the stream-emulation contract
	// pins it to 0: emulated requests ride the complete loop).
	streamCalls int
}

func (p *emulProvider) ID() string { return "emul" }
func (p *emulProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{Tools: true}
}
func (p *emulProvider) RefreshCredentials(_ context.Context) error { return nil }

func (p *emulProvider) Complete(_ context.Context, req ir.Request) (ir.Response, error) {
	p.lastReq = req
	p.calls++
	if p.calls == 1 {
		return ir.Response{
			ID:         "m1",
			StopReason: ir.StopToolUse,
			Content: []ir.Part{
				ir.ToolCallPart{ID: "tc1", Name: "web_search", Input: []byte(`{"query":"go release"}`)},
			},
		}, nil
	}
	return ir.Response{ID: "m2", StopReason: ir.StopEndTurn, Content: []ir.Part{ir.TextPart{Text: "done"}}}, nil
}

func (p *emulProvider) Stream(ctx context.Context, req ir.Request) (<-chan ir.StreamEvent, func(), error) {
	p.streamCalls++
	return nil, func() {}, nil
}

// roundTripExecutor wires an executor with the emulator enabled.
func roundTripExecutor(p provider.Provider, emu routing.Emulator) *routing.Executor {
	return routing.NewExecutor(routing.ExecutorConfig{
		Registry: regOf(p),
		Health:   routing.NewHealthTracker(),
		Emulator: emu,
	})
}

func emulatedSearchRequest() ir.Request {
	return ir.Request{
		Model: "m",
		Tools: []ir.Tool{{
			Name:        "web_search",
			InputSchema: []byte(`{"type":"object"}`),
			ServerTool:  &ir.ServerToolConfig{MaxUses: 5},
			Emulated:    true,
		}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "search the web"}}},
		},
	}
}

func TestExecute_EmulatedWebSearchRoundTrip(t *testing.T) {
	p := &emulProvider{}
	emu := &fakeEmulator{searchAnswers: []string{`[
		{"URL":"https://go.dev/dl/","Title":"Go Downloads","EncryptedContent":"","PageAge":"2026-09-01"},
		{"URL":"https://go.dev/blog/","Title":"Go Blog","EncryptedContent":"","PageAge":""}
	]`}}
	e := roundTripExecutor(p, emu)

	resp, trace, err := e.Execute(context.Background(), []routing.ChainEntry{{ProviderID: "emul"}}, emulatedSearchRequest())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The client sees the final answer plus the server-tool blocks the
	// proxy synthesized (protocol-conform: never silently dropped).
	if len(resp.Content) < 2 {
		t.Fatalf("content = %d parts, want >= 2 (tool use + result before the answer)", len(resp.Content))
	}
	use, ok := resp.Content[0].(ir.ServerToolUsePart)
	if !ok || use.Name != "web_search" {
		t.Fatalf("content[0] = %+v, want ServerToolUsePart web_search", resp.Content[0])
	}
	res, ok := resp.Content[1].(ir.ServerToolResultPart)
	if !ok || res.ToolName != "web_search" || res.IsError {
		t.Fatalf("content[1] = %+v, want ServerToolResultPart web_search", resp.Content[1])
	}
	if len(res.Search) != 2 || res.Search[0].URL != "https://go.dev/dl/" {
		t.Errorf("search results = %+v", res.Search)
	}
	if res.Search[0].Title != "Go Downloads" || res.Search[0].PageAge != "2026-09-01" {
		t.Errorf("search[0] fields = %+v", res.Search[0])
	}

	// Usage accounting is honest: one search request counted.
	if resp.Usage.WebSearchRequests != 1 {
		t.Errorf("usage.web_search_requests = %d, want 1", resp.Usage.WebSearchRequests)
	}

	// The second dispatch carried the tool result the emulator produced.
	if p.calls != 2 {
		t.Fatalf("provider calls = %d, want 2 (tool call → result → answer)", p.calls)
	}
	if len(p.lastReq.Messages) == 0 {
		t.Fatal("second dispatch lost all messages")
	}
	lastMsg := p.lastReq.Messages[len(p.lastReq.Messages)-1]
	var tr *ir.ToolResultPart
	for _, part := range lastMsg.Parts {
		if r, ok := part.(ir.ToolResultPart); ok {
			tr = &r
		}
	}
	if tr == nil {
		t.Fatalf("second dispatch has no tool result: %+v", p.lastReq.Messages)
	}
	if tr.ToolUseID != "tc1" || tr.IsError {
		t.Errorf("tool result = %+v, want id tc1 non-error", tr)
	}
	// The re-dispatch must NOT carry the emulated tool def again...
	for _, tool := range p.lastReq.Tools {
		if tool.Emulated {
			// ...unless the budget still allows another call — it does
			// here (MaxUses 5, one used), so it legitimately rides.
			if tool.ServerTool == nil || tool.ServerTool.MaxUses != 5 {
				t.Errorf("re-dispatch tool def lost its knobs: %+v", tool)
			}
		}
	}

	// Emulation must be traced — never silent.
	found := false
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventEmulatedTool {
			found = true
		}
	}
	if !found {
		t.Error("trace must record the emulated-tool substitution")
	}
}

func TestExecute_EmulatedWebFetchRoundTrip(t *testing.T) {
	p := &emulFetchProvider{}
	emu := &fakeEmulator{fetchAnswers: []string{`{
		"URL":"https://example.com/article","Title":"Article Title",
		"Source":"text","MediaType":"text/plain",
		"Data":"Full text...","CitationsEnabled":true,
		"RetrievedAt":"2025-08-25T10:30:00Z"
	}`}}
	e := roundTripExecutor(p, emu)

	req := ir.Request{
		Model: "m",
		Tools: []ir.Tool{{
			Name:       "web_fetch",
			ServerTool: &ir.ServerToolConfig{MaxUses: 10},
			Emulated:   true,
		}},
		// The fetched URL must appear in prior context (security rule).
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "read https://example.com/article please"}}},
		},
	}
	resp, _, err := e.Execute(context.Background(), []routing.ChainEntry{{ProviderID: "emulfetch"}}, req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	res, ok := findFetchResult(resp)
	if !ok {
		t.Fatalf("no web_fetch result part in response: %+v", resp.Content)
	}
	if res.Fetch == nil {
		t.Fatal("fetch result without content")
	}
	if res.Fetch.URL != "https://example.com/article" || res.Fetch.Title != "Article Title" {
		t.Errorf("fetch url/title = %q/%q", res.Fetch.URL, res.Fetch.Title)
	}
	if res.Fetch.MediaType != "text/plain" || !res.Fetch.CitationsEnabled {
		t.Errorf("fetch media/citations = %q/%v", res.Fetch.MediaType, res.Fetch.CitationsEnabled)
	}
	if resp.Usage.WebFetchRequests != 1 {
		t.Errorf("usage.web_fetch_requests = %d, want 1", resp.Usage.WebFetchRequests)
	}
}

// emulFetchProvider issues a web_fetch tool call, then answers.
type emulFetchProvider struct {
	calls   int
	lastReq ir.Request
}

func (p *emulFetchProvider) ID() string { return "emulfetch" }
func (p *emulFetchProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{Tools: true}
}
func (p *emulFetchProvider) RefreshCredentials(_ context.Context) error { return nil }

func (p *emulFetchProvider) Complete(_ context.Context, req ir.Request) (ir.Response, error) {
	p.lastReq = req
	p.calls++
	if p.calls == 1 {
		return ir.Response{
			ID:         "m1",
			StopReason: ir.StopToolUse,
			Content: []ir.Part{
				ir.ToolCallPart{ID: "tc2", Name: "web_fetch", Input: []byte(`{"url":"https://example.com/article"}`)},
			},
		}, nil
	}
	return ir.Response{ID: "m2", StopReason: ir.StopEndTurn, Content: []ir.Part{ir.TextPart{Text: "fetched"}}}, nil
}

func (p *emulFetchProvider) Stream(ctx context.Context, req ir.Request) (<-chan ir.StreamEvent, func(), error) {
	return nil, func() {}, nil
}

func findFetchResult(resp ir.Response) (ir.ServerToolResultPart, bool) {
	for _, part := range resp.Content {
		if r, ok := part.(ir.ServerToolResultPart); ok && r.ToolName == "web_fetch" {
			return r, true
		}
	}
	return ir.ServerToolResultPart{}, false
}

// TestExecute_UnservableFetchIsReportedAsAnError — a scrape that produced no
// page must reach the client as the documented error, never as a success
// result whose document block carries no source.data.
//
// Firecrawl answers HTTP 200 even for a page it could not serve, so the
// adapter (pageUsable) hands back a NON-NIL WebFetchContent with an empty
// Data, and an all-empty account walk returns it with err == nil. The
// reference enumerates that case — "url_not_accessible: Failed to fetch
// content (HTTP error)" — and every document block it describes carries
// source.data (docs.anthropic.com web-fetch-tool, read 2026-10-11).
//
// The pool's "an empty answer outranks a later failure" rule is untouched:
// that decides which ACCOUNT serves the call, not what the client is told.
func TestExecute_UnservableFetchIsReportedAsAnError(t *testing.T) {
	p := &emulFetchProvider{}
	// Exactly the adapter's unusable-page answer: non-nil, no content.
	emu := &fakeEmulator{fetchAnswers: []string{`{
		"URL":"https://example.com/article",
		"Source":"text","MediaType":"text/plain",
		"RetrievedAt":"2026-10-11T12:00:00Z"
	}`}}
	e := roundTripExecutor(p, emu)

	req := ir.Request{
		Model: "m",
		Tools: []ir.Tool{{
			Name:       "web_fetch",
			ServerTool: &ir.ServerToolConfig{MaxUses: 10},
			Emulated:   true,
		}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "read https://example.com/article please"}}},
		},
	}
	resp, _, err := e.Execute(context.Background(), []routing.ChainEntry{{ProviderID: "emulfetch"}}, req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	res, ok := findFetchResult(resp)
	if !ok {
		t.Fatalf("no web_fetch result part in response: %+v", resp.Content)
	}
	if !res.IsError {
		t.Fatalf("an unservable page must be an error result, got %+v", res)
	}
	if res.ErrorCode != "url_not_accessible" {
		t.Errorf("error_code = %q, want url_not_accessible", res.ErrorCode)
	}
	if res.Fetch != nil {
		t.Errorf("an error result must carry no fetch payload, got %+v", res.Fetch)
	}
}

// TestExecute_EmulatedFetchHonorsMaxContentTokens — the reference is explicit:
// "The max_content_tokens parameter limits the amount of content included in
// the context. If the fetched content exceeds this limit, the tool truncates
// it." (docs.anthropic.com web-fetch-tool, read 2026-10-11).
//
// The budget is decoded into ir.ServerToolConfig.MaxContentTokens and was
// enforced NOWHERE, so a multi-megabyte page rode into the re-dispatch message
// at full size and the upstream answered an empty turn — the failure the
// operator sees as "No response from model". The limit applies to text, not to
// binary content, and is optional (zero = no limit).
func TestExecute_EmulatedFetchHonorsMaxContentTokens(t *testing.T) {
	const budget = 1000 // tokens
	const chars = 100_000
	big := strings.Repeat("x", chars)

	page := func() string {
		return `{"URL":"https://example.com/article","Title":"Big","Source":"text",` +
			`"MediaType":"text/plain","Data":"` + big + `","RetrievedAt":"2026-10-11T12:00:00Z"}`
	}
	run := func(t *testing.T, st *ir.ServerToolConfig) (*emulFetchProvider, ir.ServerToolResultPart) {
		t.Helper()
		p := &emulFetchProvider{}
		e := roundTripExecutor(p, &fakeEmulator{fetchAnswers: []string{page()}})
		req := ir.Request{
			Model: "m",
			Tools: []ir.Tool{{Name: "web_fetch", ServerTool: st, Emulated: true}},
			Messages: []ir.Message{
				{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "read https://example.com/article please"}}},
			},
		}
		resp, _, err := e.Execute(context.Background(), []routing.ChainEntry{{ProviderID: "emulfetch"}}, req)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		res, ok := findFetchResult(resp)
		if !ok {
			t.Fatalf("no web_fetch result part in response: %+v", resp.Content)
		}
		return p, res
	}

	t.Run("truncates to the budget", func(t *testing.T) {
		p, res := run(t, &ir.ServerToolConfig{MaxUses: 10, MaxContentTokens: budget})
		if res.IsError {
			t.Fatalf("a truncated page is still a page, got error %+v", res)
		}
		if res.Fetch == nil {
			t.Fatal("fetch payload missing")
		}
		// The repo's established ratio is 4 chars/token (ir.EstimateTokens),
		// which is also the ratio the reference's own examples use.
		if limit := 4*budget + len("…"); len(res.Fetch.Data) > limit {
			t.Errorf("fetched text = %d bytes, want at most %d (%d tokens)", len(res.Fetch.Data), limit, budget)
		}
		// The re-dispatch message is the thing that actually reached the
		// upstream model at full size — bound it there too.
		if n := len(p.lastReq.Messages); n > 0 {
			for _, msg := range p.lastReq.Messages {
				for _, part := range msg.Parts {
					if tr, ok := part.(ir.ToolResultPart); ok {
						for _, sub := range tr.Content {
							if txt, ok := sub.(ir.TextPart); ok && len(txt.Text) > 10_000 {
								t.Errorf("re-dispatch tool result is %d bytes — the budget did not reach it", len(txt.Text))
							}
						}
					}
				}
			}
		}
	})

	t.Run("an absurd budget is not a panic", func(t *testing.T) {
		// max_content_tokens arrives from the client. A value near MaxInt would
		// overflow the chars-per-token multiply into a NEGATIVE byte limit,
		// which truncateRunes would then use as a slice index — a panic out of
		// untrusted input. 1<<61 × 4 wraps to exactly MinInt64.
		_, res := run(t, &ir.ServerToolConfig{MaxUses: 10, MaxContentTokens: 1 << 61})
		if res.IsError {
			t.Fatalf("a page under an absurd budget is still a page, got %+v", res)
		}
		if res.Fetch == nil {
			t.Fatal("fetch payload missing")
		}
		// A budget far above the page cannot truncate it.
		if len(res.Fetch.Data) != chars {
			t.Errorf("fetched text = %d bytes, want the page untouched (%d)", len(res.Fetch.Data), chars)
		}
	})

	t.Run("zero means no limit", func(t *testing.T) {
		// The parameter is optional; an absent budget must not invent one.
		_, res := run(t, &ir.ServerToolConfig{MaxUses: 10})
		if res.Fetch == nil {
			t.Fatal("fetch payload missing")
		}
		if len(res.Fetch.Data) != chars {
			t.Errorf("fetched text = %d bytes, want the page untouched (%d)", len(res.Fetch.Data), chars)
		}
	})
}

func TestExecute_EmulatedFetchRejectsURLNotInContext(t *testing.T) {
	// Security rule: web_fetch may only fetch URLs
	// that appeared in prior conversation context — never a URL the model
	// invented (SSRF/prompt-injection guard). Violations produce a
	// protocol-conform error result (url_not_in_prior_context), not an
	// upstream error, and the loop continues so the model can recover.
	p := &urlNotInContextProvider{}
	e := roundTripExecutor(p, &fakeEmulator{})

	req := ir.Request{
		Model: "m",
		Tools: []ir.Tool{{
			Name:       "web_fetch",
			ServerTool: &ir.ServerToolConfig{MaxUses: 10},
			Emulated:   true,
		}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "fetch something"}}},
		},
	}
	resp, _, err := e.Execute(context.Background(), []routing.ChainEntry{{ProviderID: "emulref"}}, req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	res, ok := findFetchResult(resp)
	if !ok {
		t.Fatalf("no web_fetch result in response: %+v", resp.Content)
	}
	if !res.IsError || res.ErrorCode != "url_not_in_prior_context" {
		t.Errorf("result = error?%v code=%q, want url_not_in_prior_context", res.IsError, res.ErrorCode)
	}
	if p.calls < 2 {
		t.Errorf("provider calls = %d, want >= 2 (error result must re-dispatch so the model can recover)", p.calls)
	}
}

type urlNotInContextProvider struct {
	calls int
}

func (p *urlNotInContextProvider) ID() string { return "emulref" }
func (p *urlNotInContextProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{Tools: true}
}
func (p *urlNotInContextProvider) RefreshCredentials(_ context.Context) error { return nil }

func (p *urlNotInContextProvider) Complete(_ context.Context, req ir.Request) (ir.Response, error) {
	p.calls++
	if p.calls == 1 {
		return ir.Response{
			ID:         "m1",
			StopReason: ir.StopToolUse,
			Content: []ir.Part{
				ir.ToolCallPart{ID: "tc3", Name: "web_fetch", Input: []byte(`{"url":"https://evil.example/steal"}`)},
			},
		}, nil
	}
	return ir.Response{ID: "m2", StopReason: ir.StopEndTurn, Content: []ir.Part{ir.TextPart{Text: "cannot fetch"}}}, nil
}

func (p *urlNotInContextProvider) Stream(ctx context.Context, req ir.Request) (<-chan ir.StreamEvent, func(), error) {
	return nil, func() {}, nil
}

func TestExecute_EmulatedMaxUsesExceeded(t *testing.T) {
	// max_uses exceeded → error result block with error_code
	// max_uses_exceeded, never an upstream error, and the loop
	// continues so the model sees the budget exhaustion and answers.
	//
	// Spec note: max_uses=N allows N invocations; the (N+1)th call is refused
	// with max_uses_exceeded (fixture server-tool-web-search-response.json).
	// With max: 1 the model issues exactly ONE call, which MaxUses=1
	// legitimately allows — so
	// the provider must call twice (max: 2) for the SECOND call to hit the
	// budget and produce the error result.
	p := &loopingProvider{answer: `{"query":"q"}`, name: "web_search", max: 2}
	e := roundTripExecutor(p, &fakeEmulator{})

	req := emulatedSearchRequest()
	req.Tools[0].ServerTool.MaxUses = 1

	resp, _, err := e.Execute(context.Background(), []routing.ChainEntry{{ProviderID: "loop"}}, req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var sawExceeded bool
	for _, part := range resp.Content {
		if r, ok := part.(ir.ServerToolResultPart); ok && r.IsError && r.ErrorCode == "max_uses_exceeded" {
			sawExceeded = true
		}
	}
	if !sawExceeded {
		t.Fatalf("no max_uses_exceeded error result: %+v", resp.Content)
	}
}

// loopingProvider keeps issuing the tool call until it has been called
// max times, then answers (models that refuse to give up).
type loopingProvider struct {
	answer string
	name   string
	max    int
	calls  int
}

func (p *loopingProvider) ID() string { return "loop" }
func (p *loopingProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{Tools: true}
}
func (p *loopingProvider) RefreshCredentials(_ context.Context) error { return nil }

func (p *loopingProvider) Complete(_ context.Context, req ir.Request) (ir.Response, error) {
	p.calls++
	if p.calls <= p.max {
		return ir.Response{
			ID:         "m" + itoa(p.calls),
			StopReason: ir.StopToolUse,
			Content: []ir.Part{
				ir.ToolCallPart{ID: "tc" + itoa(p.calls), Name: p.name, Input: []byte(p.answer)},
			},
		}, nil
	}
	return ir.Response{ID: "final", StopReason: ir.StopEndTurn, Content: []ir.Part{ir.TextPart{Text: "done"}}}, nil
}

func (p *loopingProvider) Stream(ctx context.Context, req ir.Request) (<-chan ir.StreamEvent, func(), error) {
	return nil, func() {}, nil
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

// TestExecute_EmulatorNilAnswersUnavailable: the CLAUDE.md degrade
// contract: "no Firecrawl key → web-tool emulation returns documented
// 'unavailable' errors". With the emulate policy applied (tool def carries
// the Emulated marker) but no emulator configured, a model tool call against
// the emulated tool must be answered with a protocol-conform error result
// (error_code "unavailable") and the loop re-dispatches so the model can
// recover — never passing the unexecutable tool call through to the client.
func TestExecute_EmulatorNilAnswersUnavailable(t *testing.T) {
	p := &emulProvider{}
	e := roundTripExecutor(p, nil) // policy emulate, no emulator configured

	resp, trace, err := e.Execute(context.Background(), []routing.ChainEntry{{ProviderID: "emul"}}, emulatedSearchRequest())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var res *ir.ServerToolResultPart
	for _, part := range resp.Content {
		if r, ok := part.(ir.ServerToolResultPart); ok && r.ToolName == "web_search" {
			res = &r
		}
	}
	if res == nil {
		t.Fatalf("no web_search result part in response (pass-through): %+v", resp.Content)
	}
	if !res.IsError || res.ErrorCode != "unavailable" {
		t.Errorf("result = error?%v code=%q, want unavailable", res.IsError, res.ErrorCode)
	}

	// The unavailable result must be re-dispatched so the model can
	// recover (same loop discipline as url_not_in_prior_context), and the
	// re-dispatched ToolResultPart carries the documented error code.
	if p.calls < 2 {
		t.Fatalf("provider calls = %d, want >= 2 (error result must re-dispatch)", p.calls)
	}
	lastMsg := p.lastReq.Messages[len(p.lastReq.Messages)-1]
	var tr *ir.ToolResultPart
	for _, part := range lastMsg.Parts {
		if r, ok := part.(ir.ToolResultPart); ok {
			tr = &r
		}
	}
	if tr == nil {
		t.Fatalf("re-dispatch carries no tool result: %+v", p.lastReq.Messages)
	}
	if !tr.IsError || len(tr.Content) == 0 {
		t.Fatalf("re-dispatched tool result = %+v, want error result with content", tr)
	}
	if txt, ok := tr.Content[0].(ir.TextPart); !ok || txt.Text != `{"error_code":"unavailable"}` {
		t.Errorf("re-dispatched tool result content = %+v, want {\"error_code\":\"unavailable\"}", tr.Content[0])
	}

	// Emulated-tool answers must be traced — never silent.
	found := false
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventEmulatedTool {
			found = true
		}
	}
	if !found {
		t.Error("trace must record the emulated-tool answer")
	}
}
