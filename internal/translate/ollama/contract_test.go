package ollama_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/translate/ir"
	ol "github.com/tubeof/proxyllm/internal/translate/ollama"
	"github.com/tubeof/proxyllm/internal/translate/testutil"
)

// Contract tests for the Ollama native /api/chat codec, RED-GREEN.
// Every assertion derives from the golden fixtures in
// testdata/golden/ollama/ (provenance: docs.ollama.com,
// accessed 2026-09-10).

func TestDecodeRequest_ChatRequest(t *testing.T) {
	raw := testutil.LoadGolden(t, "ollama/chat-request.json")
	req, err := ol.Codec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}

	if req.Model != "llama3.3" {
		t.Errorf("model = %q, want llama3.3", req.Model)
	}
	if req.Stream {
		t.Error("stream:false must decode to false")
	}
	// System role message decodes into System[0].
	if len(req.System) != 1 || req.System[0].Text != "You are a helpful assistant." {
		t.Errorf("system = %+v", req.System)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(req.Messages))
	}
	if txt, ok := req.Messages[0].Parts[0].(ir.TextPart); !ok || txt.Text != "What's the weather in San Francisco?" {
		t.Errorf("messages[0] = %+v", req.Messages[0].Parts[0])
	}
	// Tools are OpenAI-function-shaped in the Ollama API.
	if len(req.Tools) != 1 || req.Tools[0].Name != "get_weather" {
		t.Fatalf("tools = %+v", req.Tools)
	}
}

func TestDecodeResponse_ChatResponse(t *testing.T) {
	raw := testutil.LoadGolden(t, "ollama/chat-response.json")
	resp, err := ol.Codec{}.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}

	if resp.ID == "" {
		t.Error("response must carry an ID (synthesized from created_at/model per L9)")
	}
	if resp.Model != "llama3.3" {
		t.Errorf("model = %q, want llama3.3", resp.Model)
	}
	if resp.StopReason != ir.StopEndTurn {
		t.Errorf("done_reason = %q, want end_turn", resp.StopReason)
	}

	// Content: thinking + text + tool_call with OBJECT arguments.
	if len(resp.Content) != 3 {
		t.Fatalf("content = %d parts, want 3 (thinking, text, tool_call)", len(resp.Content))
	}
	th, ok := resp.Content[0].(ir.ThinkingPart)
	if !ok {
		t.Fatalf("content[0] = %T, want ir.ThinkingPart", resp.Content[0])
	}
	if th.Thinking != "The user wants weather info. I should call get_weather." {
		t.Errorf("thinking = %q", th.Thinking)
	}
	if txt, ok := resp.Content[1].(ir.TextPart); !ok || txt.Text != "I'll check the weather for you." {
		t.Errorf("content[1] = %+v", resp.Content[1])
	}
	tc, ok := resp.Content[2].(ir.ToolCallPart)
	if !ok || tc.Name != "get_weather" {
		t.Fatalf("content[2] = %+v", resp.Content[2])
	}
	// Ollama tool_calls arguments are JSON objects, not strings.
	if string(tc.Input) != `{"location":"San Francisco, CA"}` && !strings.Contains(string(tc.Input), `"location"`) {
		t.Errorf("tool_call arguments = %s", tc.Input)
	}

	// Usage: prompt_eval_count/eval_count/prompt_eval_cached_count.
	if resp.Usage.InputTokens != 26 || resp.Usage.OutputTokens != 283 {
		t.Errorf("usage in/out = %d/%d, want 26/283", resp.Usage.InputTokens, resp.Usage.OutputTokens)
	}
	if resp.Usage.CacheReadInputTokens != 0 {
		t.Errorf("usage.cache_read_input_tokens = %d, want 0", resp.Usage.CacheReadInputTokens)
	}
}

func TestStreamDecode_FullSequence(t *testing.T) {
	wire := testutil.LoadGolden(t, "ollama/chat-stream.ndjson")
	want := testutil.LoadExpectedEvents(t, "ollama/chat-stream.ndjson.expected.json")

	got := testutil.DrainStreamDecoder(t, ol.NewStreamDecoder(bytes.NewReader(wire)))
	testutil.AssertEventsEqual(t, "ollama/chat-stream.ndjson", want, got)
}

// Encoding must send an EXPLICIT stream:false for non-streaming requests —
// the Ollama API defaults stream to true when the field is absent.
func TestEncodeRequest_StreamFalseExplicit(t *testing.T) {
	req := ir.Request{
		Model: "llama3.3",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "Hi"}}},
		},
		Stream: false,
	}
	raw, err := ol.Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(raw), `"stream":false`) {
		t.Errorf("encoded request must carry explicit stream:false, got %s", raw)
	}
}

// Thinking config maps to the think parameter: budget → effort level
// (high/medium/low) or boolean.
func TestEncodeRequest_ThinkMapping(t *testing.T) {
	req := ir.Request{
		Model: "llama3.3",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "Hi"}}},
		},
		Thinking: &ir.ThinkingConfig{Enabled: true, BudgetTokens: 10000},
	}
	raw, err := ol.Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(raw), `"think"`) {
		t.Errorf("thinking config must map to the think parameter, got %s", raw)
	}
}

func TestEncodeRequest_FromIR(t *testing.T) {
	req := ir.Request{
		Model:  "llama3.3",
		System: []ir.TextPart{{Text: "You are helpful."}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "Hi"}}},
		},
		Tools: []ir.Tool{{Name: "get_weather", InputSchema: []byte(`{"type":"object"}`)}},
	}
	raw, err := ol.Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	got, err := ol.Codec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest(round trip): %v", err)
	}
	if got.Model != req.Model {
		t.Errorf("round trip model = %q", got.Model)
	}
	if len(got.System) != 1 || got.System[0].Text != "You are helpful." {
		t.Errorf("round trip system = %+v", got.System)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "get_weather" {
		t.Errorf("round trip tools = %+v", got.Tools)
	}
}

// Images in tool results ride the role-agnostic `images` field the
// Ollama Message schema defines (docs/api.md, "a list of images to include
// in the message", no role restriction; accessed 2026-10-23): a tool result
// carrying an image must survive the encode instead of silently dropping
// the pixels (matrix catalog №1).
func TestEncodeRequest_ToolResultImagesRideTheMessage(t *testing.T) {
	const b64 = "aVBORw0KGgo=" // png magic, base64
	req := ir.Request{
		Model: "llama3.3",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "look"}}},
			{Role: ir.RoleAssistant, Parts: []ir.Part{ir.ToolCallPart{ID: "call_1", Name: "read_screen", Input: []byte(`{"path":"a.png"}`)}}},
			{Role: ir.RoleTool, Parts: []ir.Part{ir.ToolResultPart{
				ToolUseID: "call_1",
				Content:   []ir.Part{ir.TextPart{Text: "screenshot"}, ir.ImagePart{Source: "base64", MediaType: "image/png", Data: b64}},
			}}},
		},
	}
	raw, err := ol.Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(raw), `"images":["`+b64+`"]`) {
		t.Errorf("tool-result image must ride the message images field, got %s", raw)
	}
	// And back: decode returns the image inside the tool result.
	got, err := ol.Codec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest(round trip): %v", err)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("round trip messages = %d", len(got.Messages))
	}
	tool := got.Messages[2]
	if len(tool.Parts) != 1 {
		t.Fatalf("round trip tool message parts = %d", len(tool.Parts))
	}
	tr, ok := tool.Parts[0].(ir.ToolResultPart)
	if !ok {
		t.Fatalf("round trip tool part = %T", tool.Parts[0])
	}
	var images int
	for _, p := range tr.Content {
		if img, ok := p.(ir.ImagePart); ok {
			images++
			if img.Data != b64 {
				t.Errorf("round trip tool-result image data = %q", img.Data)
			}
		}
	}
	if images != 1 {
		t.Errorf("round trip tool result must carry the image, got %d image parts", images)
	}
}

func TestEncodeResponse_FromIR(t *testing.T) {
	resp := ir.Response{
		ID:    "ollama_test",
		Model: "llama3.3",
		Content: []ir.Part{
			ir.TextPart{Text: "Hello"},
			ir.ToolCallPart{ID: "call_1", Name: "get_weather", Input: []byte(`{"location":"SF"}`)},
		},
		StopReason: ir.StopToolUse,
		Usage:      ir.Usage{InputTokens: 26, OutputTokens: 283, CacheReadInputTokens: 8},
	}
	raw, err := ol.Codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	got, err := ol.Codec{}.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse(round trip): %v", err)
	}
	if got.Model != resp.Model || got.StopReason != ir.StopToolUse {
		t.Errorf("round trip model/stop_reason = %q/%q", got.Model, got.StopReason)
	}
	if len(got.Content) != 2 {
		t.Fatalf("round trip content = %d, want 2", len(got.Content))
	}
	if txt, ok := got.Content[0].(ir.TextPart); !ok || txt.Text != "Hello" {
		t.Errorf("round trip text = %+v", got.Content[0])
	}
	if got.Usage.InputTokens != 26 || got.Usage.OutputTokens != 283 || got.Usage.CacheReadInputTokens != 8 {
		t.Errorf("round trip usage = %+v", got.Usage)
	}
}
