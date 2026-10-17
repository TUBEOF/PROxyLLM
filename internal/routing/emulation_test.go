package routing_test

import (
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// Server-tool emulation policy tests (RED-GREEN). Derived from the
// emulation policy semantics (server_tool_emulation:
// native / emulate-firecrawl / off):
//
//   - native: server-tool defs ride untouched (the routed provider has
//     native support; nothing to substitute).
//   - emulate-firecrawl: each native server-tool def (web_search_* /
//     web_fetch_*) becomes an EMULATED CLIENT tool — same family name,
//     input schema for the model, knobs preserved for the executor's
//     enforcement (max_uses, domain filters) — while the dated type string
//     and server-tool knobs are stripped so no upstream sees a server-tool
//     def it would reject.
//   - off: server-tool defs are REMOVED from the request (the operator
//     opted out of emulation; a provider that cannot serve them must not
//     receive them) with a traced degradation.
//
// Non-server tools and server-tool result blocks in history are never
// touched by the policy. Every application is traced — never silent.

func emulationReq() ir.Request {
	return ir.Request{
		Model:     "m",
		MaxTokens: 1024,
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "search"}}},
		},
		Tools: []ir.Tool{
			{Name: "web_search", Type: "web_search_20250305", ServerTool: &ir.ServerToolConfig{
				MaxUses:        5,
				AllowedDomains: []string{"example.com"},
			}},
			{Name: "get_weather", InputSchema: []byte(`{"type":"object"}`)},
		},
	}
}

func TestApplyCompat_EmulationNative_LeavesToolsUntouched(t *testing.T) {
	req := emulationReq()
	got, trace, err := routing.ApplyCompat(routing.Compat{ServerToolEmulation: routing.EmulationNative}, req)
	if err != nil {
		t.Fatalf("ApplyCompat: %v", err)
	}
	if len(got.Tools) != 2 {
		t.Fatalf("tools = %d, want 2 (native policy substitutes nothing)", len(got.Tools))
	}
	if got.Tools[0].Type != "web_search_20250305" || got.Tools[0].ServerTool == nil {
		t.Errorf("native policy must not touch the server-tool def: %+v", got.Tools[0])
	}
	if got.Tools[0].Emulated {
		t.Error("native policy must not mark tools emulated")
	}
	// A no-op policy application is not traced (nothing happened).
	for _, ev := range trace.Events() {
		if ev.Type == "compat_policy" && ev.From == "server_tool_emulation" {
			t.Errorf("native policy must not emit a compat_policy event: %+v", ev)
		}
	}
}

func TestApplyCompat_EmulationEmulate_ConvertsServerToolDefs(t *testing.T) {
	req := emulationReq()
	got, trace, err := routing.ApplyCompat(routing.Compat{ServerToolEmulation: routing.EmulationEmulateFirecrawl}, req)
	if err != nil {
		t.Fatalf("ApplyCompat: %v", err)
	}
	if len(got.Tools) != 2 {
		t.Fatalf("tools = %d, want 2 (converted in place)", len(got.Tools))
	}

	st := got.Tools[0]
	if !st.Emulated {
		t.Fatal("converted server tool must be marked Emulated")
	}
	if st.Type != "" {
		t.Errorf("emulated tool must drop the dated type string (upstream would reject it), got %q", st.Type)
	}
	if st.ServerTool == nil || st.ServerTool.MaxUses != 5 || len(st.ServerTool.AllowedDomains) != 1 {
		t.Errorf("emulated tool must keep its knobs for executor enforcement: %+v", st.ServerTool)
	}
	if st.Name != "web_search" {
		t.Errorf("family identity must survive: name = %q", st.Name)
	}
	// The model needs an input schema to produce well-formed tool calls.
	if len(st.InputSchema) == 0 {
		t.Error("emulated tool must carry an input schema for the model")
	}

	if got.Tools[1].Emulated || got.Tools[1].Name != "get_weather" {
		t.Errorf("client tools must not be touched: %+v", got.Tools[1])
	}

	// Traced — never silent.
	found := false
	for _, ev := range trace.Events() {
		if ev.Type == "compat_policy" && ev.From == "server_tool_emulation" {
			found = true
		}
	}
	if !found {
		t.Error("emulate-firecrawl must record a compat_policy event")
	}
}

func TestApplyCompat_EmulationEmulate_InjectsMissingWebFetchSchema(t *testing.T) {
	// web_fetch conversion must work too (family, not just web_search).
	req := ir.Request{
		Model: "m",
		Tools: []ir.Tool{{Name: "web_fetch", Type: "web_fetch_20250910"}},
	}
	got, _, err := routing.ApplyCompat(routing.Compat{ServerToolEmulation: routing.EmulationEmulateFirecrawl}, req)
	if err != nil {
		t.Fatalf("ApplyCompat: %v", err)
	}
	if len(got.Tools) != 1 || !got.Tools[0].Emulated || got.Tools[0].Type != "" {
		t.Fatalf("web_fetch def not converted: %+v", got.Tools)
	}
}

func TestApplyCompat_EmulationOff_RemovesServerToolDefs(t *testing.T) {
	req := emulationReq()
	got, trace, err := routing.ApplyCompat(routing.Compat{ServerToolEmulation: routing.EmulationOff}, req)
	if err != nil {
		t.Fatalf("ApplyCompat: %v", err)
	}
	if len(got.Tools) != 1 {
		t.Fatalf("tools = %d, want 1 (server-tool def removed under off)", len(got.Tools))
	}
	if got.Tools[0].Name != "get_weather" {
		t.Errorf("remaining tool = %+v, want the client tool", got.Tools[0])
	}
	found := false
	for _, ev := range trace.Events() {
		if ev.Type == "compat_policy" && ev.From == "server_tool_emulation" {
			found = true
		}
	}
	if !found {
		t.Error("off policy must record a compat_policy event (degradation is never silent)")
	}
}

func TestApplyCompat_EmulationDefaultIsNative(t *testing.T) {
	// Unset policy = native ("" = native) — same as an explicit one.
	req := emulationReq()
	got, _, err := routing.ApplyCompat(routing.Compat{}, req)
	if err != nil {
		t.Fatalf("ApplyCompat: %v", err)
	}
	if len(got.Tools) != 2 || got.Tools[0].Type != "web_search_20250305" {
		t.Fatalf("default (unset) policy must behave native: %+v", got.Tools)
	}
}

// Client-tool claim (Claude Code's shape). The CLI ships WebFetch/WebSearch as
// ORDINARY client tools and fetches in the CLI itself, so a proxy that only
// understood the dated server-tool definitions would never see a web-tool
// request from it: no account walk, no log row. The claim rewrites such a
// definition into the emulated family def — same name the executor's loop
// matches, same input schema, no dated type — so the model's call is answered
// by the proxy instead of the client.
func clientWebToolReq() ir.Request {
	return ir.Request{
		Model:     "m",
		MaxTokens: 256,
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "fetch https://example.com"}}},
		},
		Tools: []ir.Tool{
			{Name: "WebFetch", Description: "Fetches content from a specified URL",
				InputSchema: []byte(`{"type":"object","properties":{"url":{"type":"string"},"prompt":{"type":"string"}}}`)},
			{Name: "get_weather", InputSchema: []byte(`{"type":"object"}`)},
		},
	}
}

func TestApplyCompat_ClaimsClientWebTools(t *testing.T) {
	req := clientWebToolReq()
	compat := routing.Compat{
		ServerToolEmulation: routing.EmulationEmulateFirecrawl,
		ClientWebTools:      map[string]string{"WebFetch": "web_fetch"},
	}
	got, trace, err := routing.ApplyCompat(compat, req)
	if err != nil {
		t.Fatalf("ApplyCompat: %v", err)
	}
	if len(got.Tools) != 2 {
		t.Fatalf("tools = %d, want 2 (claimed in place)", len(got.Tools))
	}
	claimed := got.Tools[0]
	if claimed.Name != "web_fetch" || !claimed.Emulated {
		t.Fatalf("the client tool was not claimed: %+v", claimed)
	}
	if claimed.Type != "" {
		t.Errorf("a claimed tool must not carry a server-tool type: %q", claimed.Type)
	}
	if string(claimed.InputSchema) == "" || !strings.Contains(string(claimed.InputSchema), `"url"`) {
		t.Errorf("the family input schema is missing: %s", claimed.InputSchema)
	}
	if strings.Contains(string(claimed.InputSchema), `"prompt"`) {
		t.Errorf("the client's own schema must be replaced by the family's: %s", claimed.InputSchema)
	}
	if claimed.ServerTool == nil {
		t.Error("a claimed tool needs a knob struct for the executor's enforcement")
	}
	if got.Tools[1].Name != "get_weather" || got.Tools[1].Emulated {
		t.Errorf("an unclaimed client tool must ride untouched: %+v", got.Tools[1])
	}
	var traced bool
	for _, ev := range trace.Events() {
		if ev.Type == "compat_policy" && ev.From == "server_tool_emulation" && strings.Contains(ev.Reason, "client web-tool def") {
			traced = true
		}
	}
	if !traced {
		t.Errorf("the claim is not traced: %+v", trace.Events())
	}
}

func TestApplyCompat_ClientWebToolClaimNeedsTheMapping(t *testing.T) {
	// The mapping is the surface's statement that this wire can carry the
	// family's result blocks; with no mapping (the OpenAI-shaped surfaces, and
	// the zero Compat) a client tool rides untouched.
	for name, compat := range map[string]routing.Compat{
		"no mapping":    {ServerToolEmulation: routing.EmulationEmulateFirecrawl},
		"native policy": {ServerToolEmulation: routing.EmulationNative, ClientWebTools: map[string]string{"WebFetch": "web_fetch"}},
		"emulation off": {ServerToolEmulation: routing.EmulationOff, ClientWebTools: map[string]string{"WebFetch": "web_fetch"}},
	} {
		got, _, err := routing.ApplyCompat(compat, clientWebToolReq())
		if err != nil {
			t.Fatalf("%s: ApplyCompat: %v", name, err)
		}
		if got.Tools[0].Name != "WebFetch" || got.Tools[0].Emulated {
			t.Errorf("%s: the client tool must ride untouched: %+v", name, got.Tools[0])
		}
	}
}

func TestApplyCompat_ClientWebToolClaimSkipsAPresentFamily(t *testing.T) {
	// A request that already declares the family as a dated server-tool def
	// must not end up with the family twice (the loop keys definitions by
	// name): the server-tool def converts, the client tool rides untouched.
	req := emulationReq()
	req.Tools = append(req.Tools, ir.Tool{Name: "WebSearch", InputSchema: []byte(`{"type":"object"}`)})
	compat := routing.Compat{
		ServerToolEmulation: routing.EmulationEmulateFirecrawl,
		ClientWebTools:      map[string]string{"WebSearch": "web_search"},
	}
	got, _, err := routing.ApplyCompat(compat, req)
	if err != nil {
		t.Fatalf("ApplyCompat: %v", err)
	}
	families := 0
	for _, tool := range got.Tools {
		if tool.Name == "web_search" {
			families++
		}
	}
	if families != 1 {
		t.Fatalf("web_search defs = %d, want exactly 1: %+v", families, got.Tools)
	}
	if got.Tools[len(got.Tools)-1].Name != "WebSearch" || got.Tools[len(got.Tools)-1].Emulated {
		t.Errorf("the client tool must ride untouched when the family is already present: %+v", got.Tools)
	}
}
