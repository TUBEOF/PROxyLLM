package routing_test

// The lifecycle this pins: a client like Claude Code searches FIRST, then
// fetches a URL THE SEARCH RETURNED. The fetched URL never appeared in a user
// message; its only ride into prior context is the synthesized web_search
// tool result that
// rode the round-trip's user-role message. That is exactly what
// priorContextURLs documents ("synthesized tool results ride user-role
// messages, so their URLs count for later fetches") — this test pins the
// contract end to end: search → fetch of a search-result URL must be a
// SUCCESSFUL fetch, never url_not_in_prior_context.
//
// The failure mode this guards against: the re-dispatch payload is the
// JSON-marshaled ServerToolResultPart. If the encoder ever drops the URL
// (or escapes it differently), the guard rejects the second round and
// web_fetch is dead in the field while web_search still works.

import (
	"context"
	"testing"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// searchThenFetchProvider scripts the two-round model: round 1 issues
// web_search, round 2 issues web_fetch of the URL the search returned
// (the URL from the search answer, NOT from any user message), round 3
// answers. It records whether the fetch round ever failed.
type searchThenFetchProvider struct {
	calls      int
	round2Req  ir.Request // the re-dispatch after the search
	fetchOK    bool       // the fetch result was non-error
	fetchCodes []string   // error codes seen on fetch results
}

func (p *searchThenFetchProvider) ID() string { return "stf" }
func (p *searchThenFetchProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{Tools: true}
}
func (p *searchThenFetchProvider) RefreshCredentials(_ context.Context) error { return nil }

func (p *searchThenFetchProvider) Complete(_ context.Context, req ir.Request) (ir.Response, error) {
	p.calls++
	switch p.calls {
	case 1:
		return ir.Response{
			ID:         "m1",
			StopReason: ir.StopToolUse,
			Content: []ir.Part{
				ir.ToolCallPart{ID: "t1", Name: "web_search", Input: []byte(`{"query":"example article"}`)},
			},
		}, nil
	case 2:
		p.round2Req = req
		return ir.Response{
			ID:         "m2",
			StopReason: ir.StopToolUse,
			Content: []ir.Part{
				ir.ToolCallPart{ID: "t2", Name: "web_fetch", Input: []byte(`{"url":"https://example.com/article"}`)},
			},
		}, nil
	default:
		// Record how the fetch round answered.
		for _, msg := range req.Messages {
			for _, part := range msg.Parts {
				if tr, ok := part.(ir.ToolResultPart); ok && tr.ToolUseID == "t2" {
					if tr.IsError {
						p.fetchCodes = append(p.fetchCodes, "tool-result-error")
					} else {
						p.fetchOK = true
					}
				}
			}
		}
		return ir.Response{ID: "m3", StopReason: ir.StopEndTurn, Content: []ir.Part{ir.TextPart{Text: "done"}}}, nil
	}
}

func (p *searchThenFetchProvider) Stream(ctx context.Context, req ir.Request) (<-chan ir.StreamEvent, func(), error) {
	return nil, func() {}, nil
}

func TestExecute_SearchResultURLIsFetchable(t *testing.T) {
	p := &searchThenFetchProvider{}
	emu := &fakeEmulator{
		searchAnswers: []string{`[{"URL":"https://example.com/article","Title":"Article","EncryptedContent":"","PageAge":""}]`},
		fetchAnswers:  []string{`{"URL":"https://example.com/article","Title":"Article","Source":"text","MediaType":"text/plain","Data":"body","CitationsEnabled":false,"RetrievedAt":""}`},
	}
	e := roundTripExecutor(p, emu)

	req := ir.Request{
		Model: "m",
		Tools: []ir.Tool{
			{Name: "web_search", ServerTool: &ir.ServerToolConfig{MaxUses: 5}, Emulated: true},
			{Name: "web_fetch", ServerTool: &ir.ServerToolConfig{MaxUses: 5}, Emulated: true},
		},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "research this topic"}}},
		},
	}
	resp, trace, err := e.Execute(context.Background(), []routing.ChainEntry{{ProviderID: "stf"}}, req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if p.calls < 3 {
		t.Fatalf("provider calls = %d, want >= 3 (search → fetch → answer)", p.calls)
	}
	if !p.fetchOK {
		t.Errorf("web_fetch of the search-result URL failed: codes %v — the search result URL must count as prior context", p.fetchCodes)
	}

	// The fetch result block the client sees is non-error with the page.
	var fetchRes *ir.ServerToolResultPart
	for _, part := range resp.Content {
		if r, ok := part.(ir.ServerToolResultPart); ok && r.ToolName == "web_fetch" {
			fetchRes = &r
		}
	}
	if fetchRes == nil || fetchRes.IsError {
		t.Errorf("client-facing web_fetch result missing or error: %+v", resp.Content)
	}

	// The trace records the fetch WITHOUT an error code.
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventEmulatedTool && ev.To == "web_fetch" {
			return // found, good
		}
	}
	t.Errorf("trace missing web_fetch emulated-tool event: %+v", trace.Events())
}

// The CROSS-TURN form: turn 2's history carries
// turn 1's synthesized blocks inside an ASSISTANT message (the wire
// replays them as assistant content) — the fetch of a URL from turn 1's
// search results must still pass the prior-context guard.
func TestExecute_CrossTurnFetchOfSearchResultURL(t *testing.T) {
	// The model's script: round 1 issues web_fetch directly (the URL only
	// exists in the assistant-side search-result block of the INPUT
	// history), round 2 answers. The test asserts the guard allowed it.
	p := &crossTurnFetchProvider{}
	emu := &fakeEmulator{
		fetchAnswers: []string{`{"URL":"https://example.com/article","Title":"Article","Source":"text","MediaType":"text/plain","Data":"body","CitationsEnabled":false,"RetrievedAt":""}`},
	}
	e := roundTripExecutor(p, emu)

	req := ir.Request{
		Model: "m",
		Tools: []ir.Tool{
			{Name: "web_fetch", ServerTool: &ir.ServerToolConfig{MaxUses: 5}, Emulated: true},
		},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "research this topic"}}},
			{Role: ir.RoleAssistant, Parts: []ir.Part{
				ir.ServerToolUsePart{ID: "s1", Name: "web_search", Input: []byte(`{"query":"x"}`)},
				ir.ServerToolResultPart{ToolUseID: "s1", ToolName: "web_search",
					Search: []ir.WebSearchResultItem{{URL: "https://example.com/article", Title: "Article"}}},
				ir.TextPart{Text: "Here is what I found."},
			}},
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "fetch the article please"}}},
		},
	}
	resp, _, err := e.Execute(context.Background(), []routing.ChainEntry{{ProviderID: "stf"}}, req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !p.fetchOK {
		t.Errorf("cross-turn fetch of a search-result URL failed — the assistant-side result blocks must count as prior context (codes %v)", p.fetchCodes)
	}
	var fetchRes *ir.ServerToolResultPart
	for _, part := range resp.Content {
		if r, ok := part.(ir.ServerToolResultPart); ok && r.ToolName == "web_fetch" {
			fetchRes = &r
		}
	}
	if fetchRes == nil || fetchRes.IsError {
		t.Errorf("client-facing web_fetch result missing or error: %+v", resp.Content)
	}
}

// crossTurnFetchProvider: round 1 issues the web_fetch tool call, round 2
// answers. It records how the fetch round's result answered.
type crossTurnFetchProvider struct {
	calls      int
	fetchOK    bool
	fetchCodes []string
}

func (p *crossTurnFetchProvider) ID() string { return "stf" }
func (p *crossTurnFetchProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{Tools: true}
}
func (p *crossTurnFetchProvider) RefreshCredentials(_ context.Context) error { return nil }

func (p *crossTurnFetchProvider) Complete(_ context.Context, req ir.Request) (ir.Response, error) {
	p.calls++
	if p.calls == 1 {
		return ir.Response{
			ID:         "m1",
			StopReason: ir.StopToolUse,
			Content: []ir.Part{
				ir.ToolCallPart{ID: "t2", Name: "web_fetch", Input: []byte(`{"url":"https://example.com/article"}`)},
			},
		}, nil
	}
	for _, msg := range req.Messages {
		for _, part := range msg.Parts {
			if tr, ok := part.(ir.ToolResultPart); ok && tr.ToolUseID == "t2" {
				if tr.IsError {
					p.fetchCodes = append(p.fetchCodes, "tool-result-error")
				} else {
					p.fetchOK = true
				}
			}
		}
	}
	return ir.Response{ID: "m2", StopReason: ir.StopEndTurn, Content: []ir.Part{ir.TextPart{Text: "done"}}}, nil
}

func (p *crossTurnFetchProvider) Stream(ctx context.Context, req ir.Request) (<-chan ir.StreamEvent, func(), error) {
	return nil, func() {}, nil
}

// The child-op model attribution: an emulated-tool
// op names the provider+model whose round issued the call, a failover op
// names the model its target attempted — the Logs page renders the model
// column from these.
func TestChildOps_CarryModelAttribution(t *testing.T) {
	events := []routing.Event{
		{Type: routing.EventAttempt, ProviderID: "p1", Model: "model-a"},
		{Type: routing.EventFailover, From: "p1", To: "p2", Reason: "chain failover after capability_gap"},
		{Type: routing.EventAttempt, ProviderID: "p2", Model: "model-b"},
		{Type: routing.EventEmulatedTool, ProviderID: "acc-firecrawl", To: "web_search",
			Reason: `query "q" → 3 result(s) via acc-firecrawl`, LatencyMS: 412},
	}
	ops := routing.ChildOps(events)
	if len(ops) != 2 {
		t.Fatalf("ops = %d, want 2 (failover + emulated tool)", len(ops))
	}
	if ops[0].ProviderID != "p2" || ops[0].Model != "model-b" {
		t.Errorf("failover op = %s/%s, want p2/model-b", ops[0].ProviderID, ops[0].Model)
	}
	// The emulated-tool op names where the CALL went, not which model asked for
	// it: the answer to "which web provider served this" is the emulating
	// account, and naming the chat model there made the log claim a fallback
	// that never happened.
	if ops[1].ProviderID != "acc-firecrawl" {
		t.Errorf("emulated-tool op provider = %q, want the account that served the call", ops[1].ProviderID)
	}
	if ops[1].Model != "" {
		t.Errorf("emulated-tool op model = %q, want empty (the round's model is not where the call went)", ops[1].Model)
	}
	if ops[1].LatencyMS != 412 {
		t.Errorf("emulated-tool op latency = %d, want the invocation's 412 ms", ops[1].LatencyMS)
	}
}

// The capability-fallback op names the fallback model the walk served.
func TestChildOps_CapabilityFallbackNamesFallbackModel(t *testing.T) {
	events := []routing.Event{
		{Type: routing.EventAttempt, ProviderID: "p1", Model: "text-model"},
		{Type: routing.EventCapabilityFallback, Capability: "vision", Reason: "chain exhausted"},
		{Type: routing.EventFailover, From: "p1", To: "p2", Reason: "capability fallback model"},
		{Type: routing.EventAttempt, ProviderID: "p2", Model: "vision-model"},
	}
	ops := routing.ChildOps(events)
	if len(ops) != 2 {
		t.Fatalf("ops = %d, want 2", len(ops))
	}
	cf := ops[0]
	if cf.Kind != routing.OpCapabilityFallback || cf.Detail != "vision" {
		t.Errorf("first op = %+v, want capability_fallback vision", cf)
	}
	if cf.Model != "vision-model" {
		t.Errorf("capability-fallback op model = %q, want vision-model (the walk's serving model)", cf.Model)
	}
}
