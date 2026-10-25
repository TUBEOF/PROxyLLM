package routing_test

// The mixed-response invariant: an emulated web call must never reach the
// client as a bare `tool_use` named web_fetch/web_search. Claude Code only
// knows the client names WebFetch/WebSearch, so a leaked family name produces
// "<tool_use_error>Error: No such tool available: web_fetch</tool_use_error>".
// The proxy claimed the tool; it answers it, even when the same response also
// carries calls the client must answer. The round cap and the loop deadline
// use the same rule: answer or convert, never leak.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// mixedWebProvider returns one response containing a foreign client call and
// an emulated web call, then a final answer (which must never be reached: the
// mixed turn ends the loop without a re-dispatch).
type mixedWebProvider struct {
	calls int
}

func (p *mixedWebProvider) ID() string { return "mixed" }
func (p *mixedWebProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{Tools: true}
}
func (p *mixedWebProvider) RefreshCredentials(context.Context) error { return nil }

func (p *mixedWebProvider) Complete(context.Context, ir.Request) (ir.Response, error) {
	p.calls++
	return ir.Response{ID: "m1", StopReason: ir.StopToolUse, Content: []ir.Part{
		ir.TextPart{Text: "working"},
		ir.ToolCallPart{ID: "foreign-1", Name: "client_tool", Input: json.RawMessage(`{"x":1}`)},
		ir.ToolCallPart{ID: "web-1", Name: "web_search", Input: json.RawMessage(`{"query":"go release"}`)},
	}}, nil
}

func (p *mixedWebProvider) Stream(context.Context, ir.Request) (<-chan ir.StreamEvent, func(), error) {
	return nil, func() {}, nil
}

func TestExecute_MixedEmulatedWebCallAndForeignToolAnswersWebCall(t *testing.T) {
	p := &mixedWebProvider{}
	emu := &fakeEmulator{searchAnswers: []string{`[
		{"URL":"https://go.dev/dl/","Title":"Go Downloads","EncryptedContent":"","PageAge":""}
	]`}}
	e := roundTripExecutor(p, emu)

	resp, trace, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "mixed"}}, emulatedSearchRequest())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if p.calls != 1 {
		t.Errorf("provider calls = %d, want 1 (a mixed turn is not re-dispatched)", p.calls)
	}
	if emu.searchCalls != 1 {
		t.Errorf("emulator search calls = %d, want 1 (the proxy claimed the call)", emu.searchCalls)
	}

	var sawUse, sawResult, sawForeign bool
	for _, part := range resp.Content {
		switch v := part.(type) {
		case ir.ServerToolUsePart:
			if v.Name == "web_search" && v.ID == "web-1" {
				sawUse = true
			}
		case ir.ServerToolResultPart:
			if v.ToolName == "web_search" && !v.IsError && len(v.Search) == 1 {
				sawResult = true
			}
		case ir.ToolCallPart:
			if v.Name == "web_search" {
				t.Errorf("leaked emulated web call to the client: %+v", v)
			}
			if v.Name == "client_tool" && v.ID == "foreign-1" {
				sawForeign = true
			}
		}
	}
	if !sawUse || !sawResult {
		t.Errorf("missing synthesized web_search use/result (use=%v result=%v): %+v", sawUse, sawResult, resp.Content)
	}
	if !sawForeign {
		t.Errorf("the client's own tool call did not survive: %+v", resp.Content)
	}
	if resp.Usage.WebSearchRequests != 1 {
		t.Errorf("web_search usage = %d, want 1", resp.Usage.WebSearchRequests)
	}
	foundMixed := false
	for _, ev := range eventsOf(trace, routing.EventEmulatedTool) {
		if strings.Contains(ev.Reason, "mixed response") {
			foundMixed = true
		}
	}
	if !foundMixed {
		t.Errorf("the mixed corner was not traced: %+v", eventsOf(trace, routing.EventEmulatedTool))
	}
}

func TestExecute_MixedWebAndVisionAndForeignKeepsVisionDropRule(t *testing.T) {
	main := &toolScriptProvider{id: "main", resps: []ir.Response{{
		ID:         "r1",
		StopReason: ir.StopToolUse,
		Content: []ir.Part{
			ir.ToolCallPart{ID: "foreign-1", Name: "client_tool", Input: json.RawMessage(`{"x":1}`)},
			toolCall(`{"prompt":"what error text?"}`),
			ir.ToolCallPart{ID: "web-1", Name: "web_search", Input: json.RawMessage(`{"query":"go release"}`)},
		},
	}}}
	vision := &recordingProvider{id: "vision", text: "a stack trace"}
	emu := &fakeEmulator{searchAnswers: []string{`[]`}}

	req := imageRequest("what does this error say?")
	req.Tools = append(req.Tools, ir.Tool{
		Name:        "web_search",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		ServerTool:  &ir.ServerToolConfig{MaxUses: 5},
		Emulated:    true,
	})
	e := analyzeExecutor(main, vision, blindLookup(), func(cfg *routing.ExecutorConfig) {
		cfg.Emulator = emu
	})

	resp, trace, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if main.calls != 1 {
		t.Errorf("provider calls = %d, want 1 (mixed turn, no re-dispatch)", main.calls)
	}
	if emu.searchCalls != 1 {
		t.Errorf("emulator search calls = %d, want 1", emu.searchCalls)
	}
	for _, part := range resp.Content {
		if call, ok := part.(ir.ToolCallPart); ok {
			if call.Name == "analyze_image" {
				t.Errorf("analyze_image surfaced to the client in the mixed corner: %+v", call)
			}
			if call.Name == "web_search" {
				t.Errorf("leaked emulated web call to the client: %+v", call)
			}
		}
	}
	foundVisionDrop := false
	for _, ev := range eventsOf(trace, routing.EventAnalyzeImage) {
		if strings.Contains(ev.Reason, "mixed") {
			foundVisionDrop = true
		}
	}
	if !foundVisionDrop {
		t.Errorf("the vision drop was not traced: %+v", eventsOf(trace, routing.EventAnalyzeImage))
	}
}

func TestExecute_RoundCapSynthesizesUnavailableForUnansweredWebCall(t *testing.T) {
	p := &loopingProvider{name: "web_search", answer: `{"query":"go release"}`, max: 1000}
	e := roundTripExecutor(p, &fakeEmulator{})
	req := emulatedSearchRequest()
	req.Tools[0].ServerTool = nil // no max_uses budget to exhaust first

	resp, _, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "loop"}}, req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var sawErrorResult bool
	for _, part := range resp.Content {
		if call, ok := part.(ir.ToolCallPart); ok && call.Name == "web_search" {
			t.Errorf("round cap leaked an unanswered web call to the client: %+v", call)
		}
		if res, ok := part.(ir.ServerToolResultPart); ok && res.ToolName == "web_search" &&
			res.IsError && res.ErrorCode == "unavailable" {
			sawErrorResult = true
		}
	}
	if !sawErrorResult {
		t.Errorf("round cap did not synthesize an unavailable result: %+v", resp.Content)
	}
}

func TestExecuteStream_MixedEmulatedWebCallNeverLeaksToolUsePart(t *testing.T) {
	p := &mixedWebProvider{}
	emu := &fakeEmulator{}
	e := roundTripExecutor(p, emu)

	ch, cancel, _, err := e.ExecuteStream(context.Background(),
		[]routing.ChainEntry{{ProviderID: "mixed"}}, emulatedSearchRequest())
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	defer cancel()

	var sawServerUse bool
	for ev := range ch {
		if ev.Type != ir.BlockStart || ev.Part == nil {
			continue
		}
		switch v := ev.Part.(type) {
		case ir.ToolCallPart:
			if v.Name == "web_search" || v.Name == "web_fetch" {
				t.Errorf("streamed leaked emulated call: %+v", v)
			}
		case ir.ServerToolUsePart:
			if v.Name == "web_search" {
				sawServerUse = true
			}
		}
	}
	if !sawServerUse {
		t.Error("the stream carried no server_tool_use block for the answered web call")
	}
}
