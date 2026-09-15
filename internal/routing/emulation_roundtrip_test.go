package routing_test

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/proxyllm/proxyllm/internal/provider"
	"github.com/proxyllm/proxyllm/internal/routing"
	"github.com/proxyllm/proxyllm/internal/translate/ir"
)

// Executor tool-call round-trip tests for the §6.2 Firecrawl emulation:
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

// fakeEmulator scripts the Firecrawl answers for the round-trip tests
// (Analysis F mock discipline: behavior scripted up front).
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
	// proxy synthesized (protocol-conform §6.2: never silently dropped).
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

	// Emulation must be traced — never silent (§6.2/§6.4).
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
	calls int
}

func (p *emulFetchProvider) ID() string { return "emulfetch" }
func (p *emulFetchProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{Tools: true}
}
func (p *emulFetchProvider) RefreshCredentials(_ context.Context) error { return nil }

func (p *emulFetchProvider) Complete(_ context.Context, req ir.Request) (ir.Response, error) {
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

func TestExecute_EmulatedFetchRejectsURLNotInContext(t *testing.T) {
	// Security rule (§6.2 / fixture meta): web_fetch may only fetch URLs
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
	// max_uses_exceeded (A10), never an upstream error, and the loop
	// continues so the model sees the budget exhaustion and answers.
	//
	// Spec note (documented misread fix): max_uses=N allows N invocations;
	// the (N+1)th call is refused with max_uses_exceeded (fixture
	// server-tool-web-search-response.json + A10). With max: 1 the model
	// issues exactly ONE call, which MaxUses=1 legitimately allows — so
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

// TestExecute_EmulatorNilAnswersUnavailable: 4b — the CLAUDE.md degrade
// contract: "no Firecrawl key → web-tool emulation returns documented
// 'unavailable' errors". With the emulate policy applied (tool def carries
// the Emulated marker) but no emulator configured, a model tool call against
// the emulated tool must be answered with a protocol-conform error result
// (error_code "unavailable") and the loop re-dispatches so the model can
// recover — instead of passing the unexecutable tool call through to the
// client (RED today: pass-through).
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

	// Emulated-tool answers must be traced — never silent (§6.4).
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
