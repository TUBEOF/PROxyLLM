package routing

// The Responses wire's built-in web tools must enter the same emulation
// pipeline as Anthropic's dated server tools: decode to an IR server tool,
// then convert to an emulated client tool when the target cannot serve it.

import (
	"testing"

	"github.com/tubeof/proxyllm/internal/translate/openai"
)

func TestApplyCompat_ResponsesBuiltinWebToolBecomesEmulated(t *testing.T) {
	req, err := openai.ResponsesCodec{}.DecodeRequest([]byte(
		`{"model":"gpt-5","input":"hi","tools":[{"type":"web_search_preview"}]}`,
	))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, trace, err := ApplyCompat(Compat{ServerToolEmulation: EmulationEmulateFirecrawl}, req)
	if err != nil {
		t.Fatalf("ApplyCompat: %v", err)
	}
	if trace == nil {
		t.Fatal("nil trace")
	}
	if len(got.Tools) != 1 {
		t.Fatalf("tools = %+v, want one emulated tool", got.Tools)
	}
	tool := got.Tools[0]
	if !tool.Emulated || tool.Name != "web_search" || tool.ServerToolName() != "" {
		t.Fatalf("tool = %+v, want an emulated web_search function tool", tool)
	}
}
