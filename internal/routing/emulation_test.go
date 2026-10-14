package routing_test

import (
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
