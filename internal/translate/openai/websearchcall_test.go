package openai

// web_search_call contract (RED-GREEN): the Responses surface renders an
// emulated web search as the item the real API emits — verified 2026-11-02
// against the SDK types and openresponses.org. Expectations derive from the
// official contract, never from this encoder's behaviour.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/translate/ir"
)

func TestEncodeResponse_WebSearchCallItem(t *testing.T) {
	raw, err := ResponsesCodec{}.EncodeResponse(ir.Response{
		ID:    "resp_1",
		Model: "gpt-test",
		Content: []ir.Part{
			ir.ServerToolUsePart{
				ID:    "ws_1",
				Name:  "web_search",
				Input: json.RawMessage(`{"query":"wetter köln morgen"}`),
			},
			ir.ServerToolResultPart{ToolUseID: "ws_1", ToolName: "web_search"},
			ir.TextPart{Text: "Tomorrow: 12C, dry."},
		},
	})
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	var doc struct {
		Output []struct {
			Type   string `json:"type"`
			ID     string `json:"id"`
			Status string `json:"status"`
			Action *struct {
				Type  string `json:"type"`
				Query string `json:"query"`
			} `json:"action"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	if len(doc.Output) < 2 {
		t.Fatalf("output = %+v, want the search item then the message", doc.Output)
	}
	item := doc.Output[0]
	if item.Type != "web_search_call" {
		t.Fatalf("output[0].type = %q, want web_search_call (%s)", item.Type, raw)
	}
	if item.ID != "ws_1" {
		t.Errorf("output[0].id = %q, want ws_1", item.ID)
	}
	if item.Status != "completed" {
		t.Errorf("output[0].status = %q, want completed", item.Status)
	}
	if item.Action == nil || item.Action.Type != "search" || item.Action.Query != "wetter köln morgen" {
		t.Errorf("output[0].action = %+v, want search with the model's query", item.Action)
	}
	if !strings.Contains(string(raw), `{"type":"output_text","text":"Tomorrow: 12C, dry."}`) {
		t.Errorf("the final message was lost: %s", raw)
	}
}

func TestEncodeResponse_WebSearchRoundTripsAsInput(t *testing.T) {
	// The same shape must survive input→IR→encode unchanged — the replay
	// path Codex uses when it sends the previous turn's search calls back.
	in := []byte(`{"model":"gpt-test","input":[{"type":"web_search_call","id":"ws_9","status":"completed","action":{"type":"search","query":"wetter köln"}}]}`)
	req, err := ResponsesCodec{}.DecodeRequest(in)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(req.Messages) != 0 {
		t.Fatalf("a bare web_search_call input decodes to %d messages, want 0", len(req.Messages))
	}
}
