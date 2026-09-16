package anthropic_test

import (
	"bytes"
	"strings"
	"testing"

	anth "github.com/tubeof/proxyllm/internal/translate/anthropic"
	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/testutil"
)

// Contract tests for the Anthropic Messages codec, RED-GREEN per Analysis F.
// Every assertion derives from the golden fixtures in
// testdata/golden/anthropic/ (provenance: docs.anthropic.com, accessed
// 2026-09-10, facts A1–A13).

func TestDecodeRequest_MessageRequest(t *testing.T) {
	raw := testutil.LoadGolden(t, "anthropic/message-request.json")
	req, err := anth.Codec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}

	if req.Model != "claude-sonnet-5" {
		t.Errorf("model = %q, want claude-sonnet-5", req.Model)
	}
	if req.MaxTokens != 1024 {
		t.Errorf("max_tokens = %d, want 1024", req.MaxTokens)
	}
	if req.Stream {
		t.Error("stream must decode to false")
	}
	if req.StopSequences == nil || req.StopSequences[0] != "END" {
		t.Errorf("stop_sequences = %v, want [END]", req.StopSequences)
	}

	// System must decode into block form preserving cache_control (C4, A11).
	if len(req.System) != 1 {
		t.Fatalf("system parts = %d, want 1", len(req.System))
	}
	if req.System[0].Text != "You are a helpful assistant." {
		t.Errorf("system text = %q", req.System[0].Text)
	}
	if req.System[0].CacheControl == nil || req.System[0].CacheControl.Type != "ephemeral" || req.System[0].CacheControl.TTL != "5m" {
		t.Errorf("system cache_control = %+v, want ephemeral/5m", req.System[0].CacheControl)
	}

	// Messages: user with image+text, assistant tool_use, user tool_result.
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(req.Messages))
	}
	user := req.Messages[0]
	if user.Role != ir.RoleUser {
		t.Errorf("messages[0].role = %q", user.Role)
	}
	if len(user.Parts) != 2 {
		t.Fatalf("messages[0].parts = %d, want 2 (image, text)", len(user.Parts))
	}
	img, ok := user.Parts[0].(ir.ImagePart)
	if !ok {
		t.Fatalf("messages[0].parts[0] type = %T, want ir.ImagePart", user.Parts[0])
	}
	if img.Source != "base64" || img.MediaType != "image/png" {
		t.Errorf("image source/media = %q/%q, want base64/image/png", img.Source, img.MediaType)
	}
	if img.Data == "" {
		t.Error("image base64 data missing")
	}
	txt, ok := user.Parts[1].(ir.TextPart)
	if !ok || txt.Text != "Describe this image in one sentence." {
		t.Errorf("messages[0].parts[1] = %+v", user.Parts[1])
	}

	asst := req.Messages[1]
	if asst.Role != ir.RoleAssistant || len(asst.Parts) != 1 {
		t.Fatalf("messages[1] = %+v", asst)
	}
	tc, ok := asst.Parts[0].(ir.ToolCallPart)
	if !ok {
		t.Fatalf("messages[1].parts[0] type = %T, want ir.ToolCallPart", asst.Parts[0])
	}
	if tc.ID != "toolu_01A09q90qw90lq917wa35011" || tc.Name != "get_weather" {
		t.Errorf("tool_call = %s/%s", tc.ID, tc.Name)
	}
	if string(tc.Input) == "" || !strings.Contains(string(tc.Input), "San Francisco") {
		t.Errorf("tool_call input = %s", tc.Input)
	}

	resMsg := req.Messages[2]
	tr, ok := resMsg.Parts[0].(ir.ToolResultPart)
	if !ok {
		t.Fatalf("messages[2].parts[0] type = %T, want ir.ToolResultPart", resMsg.Parts[0])
	}
	if tr.ToolUseID != "toolu_01A09q90qw90lq917wa35011" {
		t.Errorf("tool_result.tool_use_id = %q", tr.ToolUseID)
	}
	if tr.IsError {
		t.Error("tool_result.is_error must be false")
	}
	if len(tr.Content) != 1 {
		t.Fatalf("tool_result content = %d parts, want 1", len(tr.Content))
	}
	if trc, ok := tr.Content[0].(ir.TextPart); !ok || trc.Text != "15°C, wind NW at 12 km/h" {
		t.Errorf("tool_result content = %+v", tr.Content[0])
	}

	// Tools.
	if len(req.Tools) != 1 || req.Tools[0].Name != "get_weather" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	if req.ToolChoice == nil || req.ToolChoice.Type != ir.ToolChoiceAuto {
		t.Errorf("tool_choice = %+v", req.ToolChoice)
	}
}

func TestDecodeRequest_Thinking(t *testing.T) {
	raw := testutil.LoadGolden(t, "anthropic/message-request-thinking.json")
	req, err := anth.Codec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if req.Thinking == nil {
		t.Fatal("thinking must decode to non-nil")
	}
	if !req.Thinking.Enabled || req.Thinking.BudgetTokens != 10000 {
		t.Errorf("thinking = %+v, want enabled/10000", req.Thinking)
	}
}

func TestDecodeRequest_AdaptiveThinking(t *testing.T) {
	raw := testutil.LoadGolden(t, "anthropic/message-request-adaptive-thinking.json")
	req, err := anth.Codec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if req.Thinking == nil || !req.Thinking.Adaptive {
		t.Errorf("thinking = %+v, want adaptive", req.Thinking)
	}
}

func TestDecodeResponse_MessageResponse(t *testing.T) {
	raw := testutil.LoadGolden(t, "anthropic/message-response.json")
	resp, err := anth.Codec{}.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}

	if resp.ID != "msg_01XFDUDYJgAACzvnptvVoYEL" {
		t.Errorf("id = %q", resp.ID)
	}
	if resp.Model != "claude-sonnet-5" {
		t.Errorf("model = %q", resp.Model)
	}
	if resp.StopReason != ir.StopToolUse {
		t.Errorf("stop_reason = %q, want tool_use", resp.StopReason)
	}

	// Content: thinking + text + tool_use in order.
	if len(resp.Content) != 3 {
		t.Fatalf("content = %d blocks, want 3", len(resp.Content))
	}
	th, ok := resp.Content[0].(ir.ThinkingPart)
	if !ok {
		t.Fatalf("content[0] = %T, want ir.ThinkingPart", resp.Content[0])
	}
	if th.Signature == "" {
		t.Error("thinking block must carry signature (A12)")
	}
	if _, ok := resp.Content[1].(ir.TextPart); !ok {
		t.Errorf("content[1] = %T, want ir.TextPart", resp.Content[1])
	}
	tc, ok := resp.Content[2].(ir.ToolCallPart)
	if !ok || tc.Name != "get_weather" || tc.ID != "toolu_01A09q90qw90lq917wa35011" {
		t.Errorf("content[2] = %+v", resp.Content[2])
	}

	// Usage with cache fields (A11).
	if resp.Usage.InputTokens != 1250 || resp.Usage.OutputTokens != 77 {
		t.Errorf("usage in/out = %d/%d, want 1250/77", resp.Usage.InputTokens, resp.Usage.OutputTokens)
	}
	if resp.Usage.CacheReadInputTokens != 8192 {
		t.Errorf("usage.cache_read_input_tokens = %d, want 8192", resp.Usage.CacheReadInputTokens)
	}
}

func TestStreamDecode_FullSequence(t *testing.T) {
	wire := testutil.LoadGolden(t, "anthropic/stream-events.sse")
	want := testutil.LoadExpectedEvents(t, "anthropic/stream-events.expected.json")

	got := testutil.DrainStreamDecoder(t, anth.NewStreamDecoder(bytes.NewReader(wire)))
	testutil.AssertEventsEqual(t, "anthropic/stream-events.sse", want, got)
}

func TestStreamDecode_ErrorTerminates(t *testing.T) {
	wire := testutil.LoadGolden(t, "anthropic/stream-error.sse")
	want := testutil.LoadExpectedEvents(t, "anthropic/stream-error.expected.json")

	got := testutil.DrainStreamDecoder(t, anth.NewStreamDecoder(bytes.NewReader(wire)))
	testutil.AssertEventsEqual(t, "anthropic/stream-error.sse", want, got)
}

// SSE parsing rules from the WHATWG spec (facts SSE-1..SSE-7): CRLF, lone
// CR, and lone LF all terminate lines; BOM stripped; comment lines ignored
// (relayed); field values strip one leading space; multi-line data
// concatenates with LF; pending event at EOF is discarded.
func TestStreamDecode_WhatwgLineEndings(t *testing.T) {
	mixed := "event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"claude-sonnet-5\",\"usage\":{\"input_tokens\":1}}}\n\nevent: ping\rdata: {\"type\":\"ping\"}\r\revent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	got := testutil.DrainStreamDecoder(t, anth.NewStreamDecoder(strings.NewReader(mixed)))
	// Must yield the canonical sequence without losing events to line-ending
	// ambiguity: start, ping, block_start, delta, block_stop, done.
	types := make([]string, len(got))
	for i, ev := range got {
		types[i] = string(ev.Type)
	}
	wantTypes := []string{"start", "ping", "block_start", "delta", "block_stop", "done"}
	if strings.Join(types, ",") != strings.Join(wantTypes, ",") {
		t.Errorf("event types = %v, want %v", types, wantTypes)
	}
}

func TestStreamDecode_BOMStripped(t *testing.T) {
	bom := "\xEF\xBB\xBF" + "event: ping\ndata: {\"type\":\"ping\"}\n\n"
	got := testutil.DrainStreamDecoder(t, anth.NewStreamDecoder(strings.NewReader(bom)))
	if len(got) != 1 || got[0].Type != ir.Ping {
		t.Errorf("BOM stream must yield exactly one ping, got %+v", got)
	}
}

// Unknown future event types must decode without error (A5) — surfaced as
// comment events so the relay logs them and clients stay compatible.
func TestStreamDecode_UnknownEventType(t *testing.T) {
	unknown := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"claude-sonnet-5\",\"usage\":{\"input_tokens\":1}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: some_future_event\ndata: {\"type\":\"some_future_event\",\"payload\":\"opaque\"}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	got := testutil.DrainStreamDecoder(t, anth.NewStreamDecoder(strings.NewReader(unknown)))
	hasUnknown := false
	for _, ev := range got {
		if ev.Type == ir.Comment {
			hasUnknown = true
		}
		if ev.Type == ir.Error {
			t.Errorf("unknown event type must not error: %+v", ev)
		}
	}
	if !hasUnknown {
		t.Errorf("unknown event type must surface as comment event; got %+v", got)
	}
}

// Incomplete event at EOF is discarded, not dispatched (SSE-6).
func TestStreamDecode_PendingEventAtEOFDiscarded(t *testing.T) {
	truncated := "event: ping\ndata: {\"type\":\"ping\"}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0"
	got := testutil.DrainStreamDecoder(t, anth.NewStreamDecoder(strings.NewReader(truncated)))
	if len(got) != 1 || got[0].Type != ir.Ping {
		t.Errorf("pending event at EOF must be discarded; got %+v", got)
	}
}

func TestEncodeResponse_FromIR(t *testing.T) {
	resp := ir.Response{
		ID:    "msg_test",
		Model: "claude-sonnet-5",
		Content: []ir.Part{
			ir.TextPart{Text: "Hello"},
			ir.ToolCallPart{ID: "toolu_01", Name: "get_weather", Input: []byte(`{"location":"SF"}`)},
		},
		StopReason: ir.StopToolUse,
		Usage:      ir.Usage{InputTokens: 10, OutputTokens: 5, CacheReadInputTokens: 100},
	}
	raw, err := anth.Codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	// Round trip: decode what we encoded and require equality of IR meaning.
	got, err := anth.Codec{}.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse(round trip): %v", err)
	}
	if got.ID != resp.ID || got.Model != resp.Model || got.StopReason != resp.StopReason {
		t.Errorf("round trip lost id/model/stop_reason: %+v", got)
	}
	if len(got.Content) != 2 {
		t.Fatalf("round trip content = %d, want 2", len(got.Content))
	}
	if txt, ok := got.Content[0].(ir.TextPart); !ok || txt.Text != "Hello" {
		t.Errorf("round trip text block = %+v", got.Content[0])
	}
	if tc, ok := got.Content[1].(ir.ToolCallPart); !ok || tc.Name != "get_weather" || string(tc.Input) != `{"location":"SF"}` {
		t.Errorf("round trip tool_use block = %+v", got.Content[1])
	}
	if got.Usage.InputTokens != 10 || got.Usage.OutputTokens != 5 || got.Usage.CacheReadInputTokens != 100 {
		t.Errorf("round trip usage = %+v", got.Usage)
	}
}

func TestEncodeRequest_FromIR(t *testing.T) {
	f := 0.7
	req := ir.Request{
		Model:     "claude-sonnet-5",
		MaxTokens: 512,
		System:    []ir.TextPart{{Text: "You are helpful.", CacheControl: &ir.CacheControl{Type: "ephemeral"}}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "Hi"}}},
		},
		Temperature: &f,
		Tools:       []ir.Tool{{Name: "get_weather", InputSchema: []byte(`{"type":"object"}`)}},
	}
	raw, err := anth.Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	got, err := anth.Codec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest(round trip): %v", err)
	}
	if got.Model != req.Model || got.MaxTokens != 512 {
		t.Errorf("round trip model/max_tokens = %q/%d", got.Model, got.MaxTokens)
	}
	if len(got.System) != 1 || got.System[0].CacheControl == nil || got.System[0].CacheControl.Type != "ephemeral" {
		t.Errorf("round trip system cache_control lost: %+v", got.System)
	}
	if got.Temperature == nil || *got.Temperature != 0.7 {
		t.Error("round trip temperature lost")
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "get_weather" {
		t.Errorf("round trip tools = %+v", got.Tools)
	}
}
