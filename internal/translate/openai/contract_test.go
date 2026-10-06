package openai_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/translate/ir"
	oa "github.com/tubeof/proxyllm/internal/translate/openai"
	"github.com/tubeof/proxyllm/internal/translate/testutil"
)

// Contract tests for the OpenAI chat-completions codec, RED-GREEN per
// Analysis F. Every assertion derives from the golden fixtures in
// testdata/golden/openai/ (provenance: developers.openai.com, accessed
// 2026-09-10, facts O2–O4, O11, O12).

func TestDecodeRequest_ChatRequest(t *testing.T) {
	raw := testutil.LoadGolden(t, "openai/chat-request.json")
	req, err := oa.Codec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}

	if req.Model != "gpt-5" {
		t.Errorf("model = %q, want gpt-5", req.Model)
	}
	if req.MaxTokens != 1024 {
		t.Errorf("max_completion_tokens = %d, want 1024", req.MaxTokens)
	}
	if req.Stream {
		t.Error("stream must decode to false")
	}

	// System role message decodes into System[0] (O2).
	if len(req.System) != 1 || req.System[0].Text != "You are a helpful assistant." {
		t.Errorf("system = %+v, want one text part", req.System)
	}

	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (user, assistant, tool)", len(req.Messages))
	}

	// User: text + image_url with detail hint (O11).
	user := req.Messages[0]
	if user.Role != ir.RoleUser || len(user.Parts) != 2 {
		t.Fatalf("messages[0] = %+v", user)
	}
	if txt, ok := user.Parts[0].(ir.TextPart); !ok || txt.Text != "What's in this image?" {
		t.Errorf("messages[0].parts[0] = %+v", user.Parts[0])
	}
	img, ok := user.Parts[1].(ir.ImagePart)
	if !ok {
		t.Fatalf("messages[0].parts[1] type = %T, want ir.ImagePart", user.Parts[1])
	}
	if img.Source != "url" || img.URL != "https://example.com/cat.png" || img.Detail != "high" {
		t.Errorf("image = %+v, want url source/detail high", img)
	}

	// Assistant: tool_calls with JSON-string arguments (O3).
	asst := req.Messages[1]
	if asst.Role != ir.RoleAssistant || len(asst.Parts) != 1 {
		t.Fatalf("messages[1] = %+v", asst)
	}
	tc, ok := asst.Parts[0].(ir.ToolCallPart)
	if !ok {
		t.Fatalf("messages[1].parts[0] type = %T, want ir.ToolCallPart", asst.Parts[0])
	}
	if tc.ID != "call_abc123" || tc.Name != "get_weather" {
		t.Errorf("tool_call = %s/%s", tc.ID, tc.Name)
	}
	if !strings.Contains(string(tc.Input), "San Francisco") {
		t.Errorf("tool_call arguments = %s", tc.Input)
	}

	// Tool result message decodes into a ToolResultPart.
	toolMsg := req.Messages[2]
	if len(toolMsg.Parts) != 1 {
		t.Fatalf("messages[2].parts = %d, want 1", len(toolMsg.Parts))
	}
	tr, ok := toolMsg.Parts[0].(ir.ToolResultPart)
	if !ok {
		t.Fatalf("messages[2].parts[0] type = %T, want ir.ToolResultPart", toolMsg.Parts[0])
	}
	if tr.ToolUseID != "call_abc123" {
		t.Errorf("tool result tool_call_id = %q", tr.ToolUseID)
	}
	if len(tr.Content) != 1 {
		t.Fatalf("tool result content = %d parts, want 1", len(tr.Content))
	}
	if trc, ok := tr.Content[0].(ir.TextPart); !ok || trc.Text != "15°C, wind NW at 12 km/h" {
		t.Errorf("tool result content = %+v", tr.Content[0])
	}

	// Tools are function-shaped (O3).
	if len(req.Tools) != 1 || req.Tools[0].Name != "get_weather" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	if req.ToolChoice == nil || req.ToolChoice.Type != ir.ToolChoiceAuto {
		t.Errorf("tool_choice = %+v", req.ToolChoice)
	}
}

func TestDecodeResponse_ChatResponse(t *testing.T) {
	raw := testutil.LoadGolden(t, "openai/chat-response.json")
	resp, err := oa.Codec{}.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}

	if resp.ID != "chatcmpl-abc123" {
		t.Errorf("id = %q", resp.ID)
	}
	if resp.Model != "gpt-5" {
		t.Errorf("model = %q", resp.Model)
	}
	// finish_reason tool_calls (O12) normalizes to the IR stop reason.
	if resp.StopReason != ir.StopToolUse {
		t.Errorf("finish_reason = %q, want tool_use", resp.StopReason)
	}

	if len(resp.Content) != 2 {
		t.Fatalf("content = %d parts, want 2 (text, tool_call)", len(resp.Content))
	}
	if txt, ok := resp.Content[0].(ir.TextPart); !ok || txt.Text != "I'll check the weather for you." {
		t.Errorf("content[0] = %+v", resp.Content[0])
	}
	tc, ok := resp.Content[1].(ir.ToolCallPart)
	if !ok || tc.ID != "call_abc123" || tc.Name != "get_weather" {
		t.Errorf("content[1] = %+v", resp.Content[1])
	}

	// Usage incl. cached_tokens → cache read (O4 details).
	if resp.Usage.InputTokens != 1250 || resp.Usage.OutputTokens != 77 {
		t.Errorf("usage in/out = %d/%d, want 1250/77", resp.Usage.InputTokens, resp.Usage.OutputTokens)
	}
	if resp.Usage.CacheReadInputTokens != 8192 {
		t.Errorf("usage.cache_read_input_tokens = %d, want 8192", resp.Usage.CacheReadInputTokens)
	}
}

func TestStreamDecode_FullSequence(t *testing.T) {
	wire := testutil.LoadGolden(t, "openai/chat-stream.sse")
	want := testutil.LoadExpectedEvents(t, "openai/chat-stream.expected.json")

	got := testutil.DrainStreamDecoder(t, oa.NewStreamDecoder(bytes.NewReader(wire)))
	testutil.AssertEventsEqual(t, "openai/chat-stream.sse", want, got)
}

// OpenAI chat streaming is data-only SSE terminated by [DONE] (O4); tool
// calls assemble by index; usage arrives in a final chunk when
// stream_options.include_usage is set.
func TestStreamDecode_DataOnlyAndDoneSentinel(t *testing.T) {
	// Event fields must be absent; only data lines carry content.
	wire := "data: {\"id\":\"c1\",\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"c1\",\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"c1\",\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	got := testutil.DrainStreamDecoder(t, oa.NewStreamDecoder(strings.NewReader(wire)))
	types := make([]string, len(got))
	for i, ev := range got {
		types[i] = string(ev.Type)
	}
	if strings.Join(types, ",") != "start,block_start,delta,block_stop,message_delta,done" {
		t.Errorf("event types = %v", types)
	}
}

// One chunk may carry several tool_calls entries — parallel tool calls
// (O4): each entry assembles by its OWN index, id/name riding the first
// entry of each index. The decoder must open one ToolCallPart block per
// index, not drop every entry after the first.
func TestStreamDecode_ParallelToolCalls(t *testing.T) {
	wire := "data: {\"id\":\"c1\",\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[" +
		"{\"index\":0,\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}," +
		"{\"index\":1,\"id\":\"call_b\",\"type\":\"function\",\"function\":{\"name\":\"get_time\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[" +
		"{\"index\":0,\"function\":{\"arguments\":\"{\\\"city\\\":\\\"SF\\\"}\"}}," +
		"{\"index\":1,\"function\":{\"arguments\":\"{\\\"tz\\\":\\\"PST\\\"}\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"

	got := testutil.DrainStreamDecoder(t, oa.NewStreamDecoder(strings.NewReader(wire)))
	var starts []ir.StreamEvent
	for _, ev := range got {
		if ev.Type == ir.BlockStart {
			starts = append(starts, ev)
		}
	}
	if len(starts) != 3 {
		t.Fatalf("want 3 BlockStarts (text + 2 tool_use), got %d: %+v", len(starts), starts)
	}
	// starts[0] is the implicit text block; 1 and 2 are the tool blocks.
	for i, want := range []struct{ id, name string }{{"call_a", "get_weather"}, {"call_b", "get_time"}} {
		part, ok := starts[i+1].Part.(ir.ToolCallPart)
		if !ok {
			t.Fatalf("BlockStart %d: want ToolCallPart, got %T", i+1, starts[i+1].Part)
		}
		if part.ID != want.id || part.Name != want.name {
			t.Errorf("BlockStart %d part = %+v, want id=%q name=%q", i+1, part, want.id, want.name)
		}
	}
	// Each tool block must accumulate its own argument fragments as deltas
	// against its own index — entry index 0 → block 1, entry index 1 →
	// block 2.
	for _, want := range []struct {
		index int
		args  string
	}{
		{1, `{"city":"SF"}`},
		{2, `{"tz":"PST"}`},
	} {
		found := false
		for _, ev := range got {
			if ev.Type == ir.Delta && ev.Index == want.index &&
				ev.Delta.Kind == ir.DeltaToolInput && ev.Delta.PartialJSON == want.args {
				found = true
			}
		}
		if !found {
			t.Errorf("missing DeltaToolInput %q at index %d in %+v", want.args, want.index, got)
		}
	}
}

// OpenAI-compat thinking providers (ollama.com/v1 with glm/gpt-oss/qwen3,
// DeepSeek-style endpoints) interleave reasoning fragments with content
// deltas — delta.reasoning (Ollama, ollama/ollama#17911) or
// delta.reasoning_content (DeepSeek shape). The IR cannot re-open a closed
// block, so the decoder buffers the fragments and flushes ONE thinking
// block after the content completes: a single text block, never the
// fragmented multi-block shape that broke Claude Code rendering.
func TestStreamDecode_ReasoningInterleaved(t *testing.T) {
	wire := testutil.LoadGolden(t, "openai/chat-stream-reasoning.sse")
	want := testutil.LoadExpectedEvents(t, "openai/chat-stream-reasoning.expected.json")

	got := testutil.DrainStreamDecoder(t, oa.NewStreamDecoder(bytes.NewReader(wire)))
	testutil.AssertEventsEqual(t, "openai/chat-stream-reasoning.sse", want, got)

	// Guard the exact failure shape of the bug this fixture pins: exactly
	// one text block, exactly one thinking block, thinking after the text.
	textStarts, thinkStarts, thinkIdx := 0, 0, -1
	for _, ev := range got {
		if ev.Type != ir.BlockStart {
			continue
		}
		switch ev.Part.(type) {
		case ir.TextPart:
			textStarts++
		case ir.ThinkingPart:
			thinkStarts++
			thinkIdx = ev.Index
		}
	}
	if textStarts != 1 || thinkStarts != 1 {
		t.Errorf("block starts: %d text / %d thinking, want 1/1 (interleaved reasoning must not fragment the text)", textStarts, thinkStarts)
	}
	lastTextStop, firstThinkStart := -1, -1
	for i, ev := range got {
		if ev.Type == ir.BlockStop && ev.Index == 0 {
			lastTextStop = i
		}
		if ev.Type == ir.BlockStart && ev.Index == thinkIdx {
			firstThinkStart = i
		}
	}
	if lastTextStop == -1 || firstThinkStart == -1 || firstThinkStart < lastTextStop {
		t.Errorf("thinking block must open after the text block closes (text stop at %d, thinking start at %d)", lastTextStop, firstThinkStart)
	}
}

// Non-streaming reasoning response (fixture chat-response-reasoning.json,
// message.reasoning_content — the Ollama asymmetry note in
// ollama/ollama#17911): the trace decodes as a ThinkingPart BEFORE the
// text part. delta.reasoning-content providers that use message.reasoning
// decode identically.
func TestDecodeResponse_ReasoningContent(t *testing.T) {
	raw := testutil.LoadGolden(t, "openai/chat-response-reasoning.json")
	resp, err := oa.Codec{}.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("content = %d parts, want 2 (thinking, text)", len(resp.Content))
	}
	th, ok := resp.Content[0].(ir.ThinkingPart)
	if !ok {
		t.Fatalf("content[0] type = %T, want ir.ThinkingPart", resp.Content[0])
	}
	if th.Thinking == "" {
		t.Error("content[0].thinking must carry the reasoning trace")
	}
	txt, ok := resp.Content[1].(ir.TextPart)
	if !ok || txt.Text != "The answer is 4." {
		t.Errorf("content[1] = %+v", resp.Content[1])
	}
}

// Runaway-trace guard (security review 2026-09-22): a pathological
// upstream pumping reasoning fragments beyond maxReasoningBuffer must not
// grow decoder memory unboundedly — the trace truncates, and the stream
// still terminates conformly (text block, one thinking block ≤ cap,
// message_delta, done).
func TestStreamDecode_ReasoningBufferCapped(t *testing.T) {
	frag := strings.Repeat("r", 1<<20) // 1 MiB per chunk
	var b bytes.Buffer
	b.WriteString("data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n")
	for i := 0; i < 16; i++ { // 16 MiB total, 8 MiB cap
		b.WriteString(`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"reasoning":"` + frag + `"},"finish_reason":null}]}` + "\n\n")
	}
	b.WriteString("data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")

	got := testutil.DrainStreamDecoder(t, oa.NewStreamDecoder(bytes.NewReader(b.Bytes())))
	types := make([]string, len(got))
	for i, ev := range got {
		types[i] = string(ev.Type)
	}
	if strings.Join(types, ",") != "start,block_start,block_stop,block_start,delta,block_stop,message_delta,done" {
		t.Errorf("event types = %v", types)
	}
	var thinkLen int
	for _, ev := range got {
		if ev.Type == ir.Delta && ev.Delta.Kind == ir.DeltaThinking {
			thinkLen = len(ev.Delta.Text)
		}
	}
	if thinkLen > 8<<20 {
		t.Errorf("thinking delta = %d bytes, want <= 8 MiB cap", thinkLen)
	}
	if thinkLen == 0 {
		t.Error("capped trace must still flush the buffered prefix")
	}
}

func TestEncodeResponse_FromIR(t *testing.T) {
	resp := ir.Response{
		ID:    "chatcmpl_test",
		Model: "gpt-5",
		Content: []ir.Part{
			ir.TextPart{Text: "Hello"},
			ir.ToolCallPart{ID: "call_1", Name: "get_weather", Input: []byte(`{"location":"SF"}`)},
		},
		StopReason: ir.StopToolUse,
		Usage:      ir.Usage{InputTokens: 10, OutputTokens: 5, CacheReadInputTokens: 100},
	}
	raw, err := oa.Codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	got, err := oa.Codec{}.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse(round trip): %v", err)
	}
	if got.ID != resp.ID || got.Model != resp.Model || got.StopReason != ir.StopToolUse {
		t.Errorf("round trip lost id/model/stop_reason: %+v", got)
	}
	if len(got.Content) != 2 {
		t.Fatalf("round trip content = %d, want 2", len(got.Content))
	}
	if txt, ok := got.Content[0].(ir.TextPart); !ok || txt.Text != "Hello" {
		t.Errorf("round trip text = %+v", got.Content[0])
	}
	if tc, ok := got.Content[1].(ir.ToolCallPart); !ok || tc.Name != "get_weather" || string(tc.Input) != `{"location":"SF"}` {
		t.Errorf("round trip tool_call = %+v", got.Content[1])
	}
	if got.Usage.InputTokens != 10 || got.Usage.OutputTokens != 5 || got.Usage.CacheReadInputTokens != 100 {
		t.Errorf("round trip usage = %+v", got.Usage)
	}
}

func TestEncodeRequest_FromIR(t *testing.T) {
	f := 0.7
	req := ir.Request{
		Model:     "gpt-5",
		MaxTokens: 512,
		System:    []ir.TextPart{{Text: "You are helpful."}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "Hi"}}},
		},
		Temperature: &f,
		Tools:       []ir.Tool{{Name: "get_weather", InputSchema: []byte(`{"type":"object"}`)}},
	}
	raw, err := oa.Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	got, err := oa.Codec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest(round trip): %v", err)
	}
	if got.Model != req.Model || got.MaxTokens != 512 {
		t.Errorf("round trip model/max_tokens = %q/%d", got.Model, got.MaxTokens)
	}
	if len(got.System) != 1 || got.System[0].Text != "You are helpful." {
		t.Errorf("round trip system = %+v", got.System)
	}
	if got.Temperature == nil || *got.Temperature != 0.7 {
		t.Error("round trip temperature lost")
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "get_weather" {
		t.Errorf("round trip tools = %+v", got.Tools)
	}
}

// TestEncodeRequest_ToolCallOnlyAssistantEncodesEmptyStringContent: an
// assistant turn carrying only a tool call is the standard shape of
// every agent loop (Claude Code sends tool_use-only assistant turns
// back as history). The encoded wire body MUST carry "content": "" —
// never "content": null: Ollama's OpenAI-compatible endpoint rejects
// null content with HTTP 400 "invalid message content type: <nil>"
// (ollama/openai/openai.go, fmt "invalid message content type: %T" on
// the nil interface; repro: magnitudedev/magnitude#42 table row
// {"role":"assistant","content":null} → 400, and mnfst/manifest#2721
// on ollama-cloud). Empty string decodes to zero parts exactly like
// null, so the round trip is invariant (partsFromContent skips "").
func TestEncodeRequest_ToolCallOnlyAssistantEncodesEmptyStringContent(t *testing.T) {
	req := ir.Request{
		Model: "glm-5.3",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "Weather in SF?"}}},
			{Role: ir.RoleAssistant, Parts: []ir.Part{
				ir.ToolCallPart{ID: "toolu_1", Name: "get_weather", Input: []byte(`{"location":"SF"}`)},
			}},
		},
	}
	raw, err := oa.Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	body := string(raw)
	if strings.Contains(body, `"content":null`) {
		t.Errorf("encoded body contains \"content\":null — Ollama's OpenAI-compat endpoint answers 400 invalid message content type: <nil>: %s", body)
	}
	if !strings.Contains(body, `"content":""`) {
		t.Errorf("encoded body must carry empty-string content for the tool-call-only assistant turn: %s", body)
	}

	// Round trip invariance: "" and null both decode to zero parts.
	got, err := oa.Codec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest(round trip): %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("round trip messages = %d, want 2", len(got.Messages))
	}
	assistant := got.Messages[1]
	if assistant.Role != ir.RoleAssistant || len(assistant.Parts) != 1 {
		t.Errorf("round trip assistant = %+v, want role assistant with the 1 tool call part", assistant)
	}
	if tc, ok := assistant.Parts[0].(ir.ToolCallPart); !ok || tc.Name != "get_weather" || tc.ID != "toolu_1" {
		t.Errorf("round trip tool call = %+v", assistant.Parts[0])
	}
}

// The anchoring tool_calls delta may carry the COMPLETE arguments string
// (spec O4: arguments arrive as appends to the same index — and upstreams
// like ollama.com/v1 send the whole string in one delta). The decoder must
// queue the initial fragment for the NEWLY opened block too; dropping it
// produces a tool_use with empty input, which clients reject as invalid
// tool parameters (bug pinned 2026-10-06, WebSearch screenshot).
func TestStreamDecode_ToolCallOneShotArgs(t *testing.T) {
	wire := testutil.LoadGolden(t, "openai/chat-stream-tool-oneshot.sse")
	want := testutil.LoadExpectedEvents(t, "openai/chat-stream-tool-oneshot.expected.json")

	got := testutil.DrainStreamDecoder(t, oa.NewStreamDecoder(bytes.NewReader(wire)))
	testutil.AssertEventsEqual(t, "openai/chat-stream-tool-oneshot.sse", want, got)
}

// Inverse direction: an IR BlockStart whose ToolCallPart carries a
// complete Input (assembled upstream of encoding) must emit its full
// arguments in the ANCHOR chunk — OpenAI clients re-issuing from the
// first delta must not see arguments:"" silently replacing the payload.
func TestStreamEncode_ToolCallAnchorWithArgs(t *testing.T) {
	var b bytes.Buffer
	enc := oa.NewStreamEncoder(&b)
	events := []ir.StreamEvent{
		{Type: ir.Start, Response: &ir.Response{ID: "r1", Model: "m1"}},
		{Type: ir.BlockStart, Index: 1, Part: ir.ToolCallPart{
			ID: "call_1", Name: "get_weather", Input: json.RawMessage(`{"city":"SF"}`),
		}},
		{Type: ir.BlockStop, Index: 1},
		{Type: ir.MessageDelta, MessageDelta: &ir.MessageDeltaPayload{StopReason: ir.StopToolUse}},
		{Type: ir.Done},
	}
	for _, ev := range events {
		if err := enc.Write(t.Context(), ev); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	out := b.String()
	// The anchor chunk must carry the arguments; a lone "" loses them.
	if !strings.Contains(out, `\"city\":\"SF\"`) && !strings.Contains(out, `{"city":"SF"}`) {
		t.Errorf("anchor chunk must carry the complete arguments, got:\n%s", out)
	}
	// Argument fragments (DeltaToolInput) still ride their own chunk
	// afterwards; nothing about the fragment shape changes.
}
