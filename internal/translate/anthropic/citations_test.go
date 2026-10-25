package anthropic_test

// Claude Code replays assistant text blocks with their citations array
// (`citations: [{type:"char_location", …}]`) in later requests. The Anthropic
// wire uses the same key for two different shapes: the text-citation ARRAY on
// a text block, and the document-source CONFIG OBJECT (`{"enabled":true}`) on
// a tool definition. A decoder that pins the object shape turns ordinary
// replay into a 500 — the production report this test reproduces.

import (
	"testing"

	anth "github.com/tubeof/proxyllm/internal/translate/anthropic"
	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/testutil"
)

func TestDecodeRequest_AcceptsTextCitationArray(t *testing.T) {
	raw := testutil.LoadGolden(t, "anthropic/message-request-citations.json")
	req, err := anth.Codec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest with a text citation array: %v", err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(req.Messages))
	}
	part, ok := req.Messages[0].Parts[0].(ir.TextPart)
	if !ok || part.Text != "The answer is 42." {
		t.Fatalf("first part = %+v, want the cited text", req.Messages[0].Parts[0])
	}
}

func TestDecodeRequest_AcceptsDocumentCitationsObject(t *testing.T) {
	raw := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 32,
		"messages": [{"role": "user", "content": "search"}],
		"tools": [{"type": "web_search_20250305", "name": "web_search",
			"citations": {"enabled": true}}]
	}`)
	req, err := anth.Codec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest with a document citations object: %v", err)
	}
	if len(req.Tools) != 1 || req.Tools[0].ServerTool == nil ||
		!req.Tools[0].ServerTool.CitationsEnabled {
		t.Fatalf("tool = %+v, want citations enabled", req.Tools)
	}
}
