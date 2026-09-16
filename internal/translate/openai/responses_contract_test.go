package openai_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/translate/ir"
	oa "github.com/tubeof/proxyllm/internal/translate/openai"
	"github.com/tubeof/proxyllm/internal/translate/testutil"
)

// Contract tests for the OpenAI Responses-API codec, RED-GREEN per Analysis
// F. Every assertion derives from the golden fixtures in
// testdata/golden/openai/responses-* (developers.openai.com, accessed
// 2026-09-10, facts O5–O8) and the streaming reference
// (developers.openai.com/api/docs/api-reference/responses-streaming,
// accessed 2026-09-10) for the inline O7 cases.

func TestResponsesDecodeRequest(t *testing.T) {
	raw := testutil.LoadGolden(t, "openai/responses-request.json")
	req, err := oa.ResponsesCodec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}

	if req.Model != "gpt-5" {
		t.Errorf("model = %q, want gpt-5", req.Model)
	}
	if req.MaxTokens != 1024 {
		t.Errorf("max_output_tokens = %d, want 1024", req.MaxTokens)
	}
	if req.Stream {
		t.Error("stream must decode to false")
	}

	// instructions is the Responses system prompt (O5).
	if len(req.System) != 1 || req.System[0].Text != "You are a helpful assistant." {
		t.Errorf("system = %+v, want one text part from instructions", req.System)
	}

	// input items map onto messages in order (O5).
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (user, function_call, function_call_output)", len(req.Messages))
	}

	user := req.Messages[0]
	if user.Role != ir.RoleUser || len(user.Parts) != 1 {
		t.Fatalf("messages[0] = %+v", user)
	}
	if txt, ok := user.Parts[0].(ir.TextPart); !ok || txt.Text != "What's the weather in San Francisco?" {
		t.Errorf("messages[0].parts[0] = %+v", user.Parts[0])
	}

	// function_call input item → assistant message with a ToolCallPart;
	// call_id is the tool call ID, arguments the JSON string (O5).
	fc := req.Messages[1]
	if fc.Role != ir.RoleAssistant || len(fc.Parts) != 1 {
		t.Fatalf("messages[1] = %+v", fc)
	}
	tc, ok := fc.Parts[0].(ir.ToolCallPart)
	if !ok {
		t.Fatalf("messages[1].parts[0] type = %T, want ir.ToolCallPart", fc.Parts[0])
	}
	if tc.ID != "call_abc123" || tc.Name != "get_weather" {
		t.Errorf("function_call id/name = %q/%q", tc.ID, tc.Name)
	}
	if string(tc.Input) != `{"location":"San Francisco"}` {
		t.Errorf("function_call arguments = %s", tc.Input)
	}

	// function_call_output item → role:tool message with a ToolResultPart
	// keyed by call_id (O5).
	out := req.Messages[2]
	if out.Role != ir.RoleTool || len(out.Parts) != 1 {
		t.Fatalf("messages[2] = %+v", out)
	}
	tr, ok := out.Parts[0].(ir.ToolResultPart)
	if !ok {
		t.Fatalf("messages[2].parts[0] type = %T, want ir.ToolResultPart", out.Parts[0])
	}
	if tr.ToolUseID != "call_abc123" {
		t.Errorf("function_call_output call_id = %q", tr.ToolUseID)
	}
	if len(tr.Content) != 1 {
		t.Fatalf("tool result content = %d parts, want 1", len(tr.Content))
	}
	if trc, ok := tr.Content[0].(ir.TextPart); !ok || trc.Text != `{"temp_c":15,"wind":"NW at 12 km/h"}` {
		t.Errorf("tool result content = %+v", tr.Content[0])
	}

	// Tools are FLAT function tools: no nested function wrapper (O6).
	if len(req.Tools) != 1 || req.Tools[0].Name != "get_weather" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	if req.Tools[0].Description != "Get the current weather for a location" {
		t.Errorf("tool description = %q", req.Tools[0].Description)
	}
	if !strings.Contains(string(req.Tools[0].InputSchema), `"location"`) {
		t.Errorf("tool parameters = %s", req.Tools[0].InputSchema)
	}

	// tool_choice "auto" (O5).
	if req.ToolChoice == nil || req.ToolChoice.Type != ir.ToolChoiceAuto {
		t.Errorf("tool_choice = %+v", req.ToolChoice)
	}

	// reasoning.effort "medium" maps onto the IR thinking config (O5).
	if req.Thinking == nil || !req.Thinking.Enabled || req.Thinking.BudgetTokens != 0 {
		t.Errorf("reasoning = %+v, want enabled thinking with effort medium", req.Thinking)
	}
}

func TestResponsesDecodeRequest_InputString(t *testing.T) {
	// input may be a plain string instead of an item array (O5).
	wire := `{"model":"gpt-5","input":"Hello there","stream":true}`
	req, err := oa.ResponsesCodec{}.DecodeRequest([]byte(wire))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(req.Messages))
	}
	if req.Messages[0].Role != ir.RoleUser {
		t.Errorf("role = %q, want user", req.Messages[0].Role)
	}
	if txt, ok := req.Messages[0].Parts[0].(ir.TextPart); !ok || txt.Text != "Hello there" {
		t.Errorf("input text = %+v", req.Messages[0].Parts[0])
	}
	if !req.Stream {
		t.Error("stream must decode to true")
	}
}

func TestResponsesDecodeResponse(t *testing.T) {
	raw := testutil.LoadGolden(t, "openai/responses-response.json")
	resp, err := oa.ResponsesCodec{}.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}

	if resp.ID != "resp_67c9fdcecf488190bdd9a0409de3a1ec07b8b0ad4e5eb654" {
		t.Errorf("id = %q", resp.ID)
	}
	if resp.Model != "gpt-5" {
		t.Errorf("model = %q", resp.Model)
	}
	// status "completed" normalizes to the IR end_turn stop reason (O5).
	if resp.StopReason != ir.StopEndTurn {
		t.Errorf("stop reason = %q, want end_turn (status completed)", resp.StopReason)
	}

	// Output message item with an output_text part (O5).
	if len(resp.Content) != 1 {
		t.Fatalf("content = %d parts, want 1", len(resp.Content))
	}
	if txt, ok := resp.Content[0].(ir.TextPart); !ok || txt.Text != "Hi there! How can I assist you today?" {
		t.Errorf("content[0] = %+v", resp.Content[0])
	}

	// Usage {input_tokens, output_tokens, total_tokens} (O5).
	if resp.Usage.InputTokens != 37 || resp.Usage.OutputTokens != 11 {
		t.Errorf("usage in/out = %d/%d, want 37/11", resp.Usage.InputTokens, resp.Usage.OutputTokens)
	}
}

func TestResponsesStreamDecode_FullSequence(t *testing.T) {
	wire := testutil.LoadGolden(t, "openai/responses-stream.sse")
	want := testutil.LoadExpectedEvents(t, "openai/responses-stream.expected.json")

	got := testutil.DrainStreamDecoder(t, oa.NewResponsesStreamDecoder(bytes.NewReader(wire)))
	testutil.AssertEventsEqual(t, "openai/responses-stream.sse", want, got)
}

// Named semantic events beyond the golden fixture rest on the streaming
// reference (O7, developers.openai.com/api/docs/api-reference/
// responses-streaming, accessed 2026-09-10):
//   - response.function_call_arguments.delta carries
//     {delta, item_id, output_index, sequence_number, type}
//   - response.output_item.added carries the function_call item, whose
//     {call_id, name} identify the tool call
//   - error carries a FLAT {code, message, param, sequence_number, type}
func TestResponsesStreamDecode_FunctionCallArguments(t *testing.T) {
	wire := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5","status":"in_progress"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"get_weather"}}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"sequence_number":5,"delta":"{\"location\":"}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"sequence_number":6,"delta":"\"SF\"}"}`,
		``,
		`event: response.function_call_arguments.done`,
		`data: {"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"sequence_number":7,"arguments":"{\"location\":\"SF\"}"}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"location\":\"SF\"}"}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5","status":"completed","usage":{"input_tokens":10,"output_tokens":3,"total_tokens":13}}}`,
		``,
		``,
	}, "\n")

	got := testutil.DrainStreamDecoder(t, oa.NewResponsesStreamDecoder(strings.NewReader(wire)))
	kinds := make([]string, 0, len(got))
	for _, ev := range got {
		kinds = append(kinds, string(ev.Type))
	}
	// created → start; output_item.added(function_call) → block_start(tool_use);
	// argument deltas → delta(tool_input); the done events are redundant
	// snapshots (skipped); output_item.done → block_stop; completed →
	// message_delta + done.
	if strings.Join(kinds, ",") != "start,block_start,delta,delta,block_stop,message_delta,done" {
		t.Fatalf("event types = %v", kinds)
	}

	if got[1].Type != ir.BlockStart {
		t.Fatalf("unexpected")
	}
	tc, ok := got[1].Part.(ir.ToolCallPart)
	if !ok {
		t.Fatalf("block_start part type = %T, want ir.ToolCallPart", got[1].Part)
	}
	if tc.ID != "call_1" || tc.Name != "get_weather" {
		t.Errorf("tool_use id/name = %q/%q", tc.ID, tc.Name)
	}
	if got[2].Delta.Kind != ir.DeltaToolInput || got[2].Delta.PartialJSON != `{"location":` {
		t.Errorf("delta[0] = %+v", got[2].Delta)
	}
	if got[3].Delta.Kind != ir.DeltaToolInput || got[3].Delta.PartialJSON != `"SF"}` {
		t.Errorf("delta[1] = %+v", got[3].Delta)
	}
	md := got[5].MessageDelta
	if md == nil || md.StopReason != ir.StopToolUse {
		t.Errorf("message_delta stop reason = %+v, want tool_use (response ended on function calls)", md)
	}
	if md.Usage.InputTokens != 10 || md.Usage.OutputTokens != 3 {
		t.Errorf("message_delta usage = %+v", md.Usage)
	}
}

// The flat error event (O7): {code, message, param, sequence_number, type}
// maps onto the terminal canonical error.
func TestResponsesStreamDecode_ErrorEvent(t *testing.T) {
	wire := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5","status":"in_progress"}}`,
		``,
		`event: error`,
		`data: {"type":"error","code":"server_error","message":"The server had an error.","param":null,"sequence_number":3}`,
		``,
		``,
	}, "\n")

	got := testutil.DrainStreamDecoder(t, oa.NewResponsesStreamDecoder(strings.NewReader(wire)))
	if len(got) != 2 || got[1].Type != ir.Error {
		t.Fatalf("events = %+v, want start + error", got)
	}
	ue := got[1].Error
	if ue == nil || ue.Code != "server_error" || ue.Message != "The server had an error." {
		t.Errorf("error = %+v", ue)
	}
}

// response.failed carries the failed response snapshot whose error object is
// the terminal error (streaming reference, O7).
func TestResponsesStreamDecode_ResponseFailed(t *testing.T) {
	wire := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5","status":"in_progress"}}`,
		``,
		`event: response.failed`,
		`data: {"type":"response.failed","sequence_number":4,"response":{"id":"resp_1","model":"gpt-5","status":"failed","error":{"code":"rate_limit_exceeded","message":"You exceeded your quota."}}}`,
		``,
		``,
	}, "\n")

	got := testutil.DrainStreamDecoder(t, oa.NewResponsesStreamDecoder(strings.NewReader(wire)))
	if len(got) != 2 || got[1].Type != ir.Error {
		t.Fatalf("events = %+v, want start + error", got)
	}
	ue := got[1].Error
	if ue == nil || ue.Code != "rate_limit_exceeded" || ue.Message != "You exceeded your quota." {
		t.Errorf("error = %+v", ue)
	}
}

func TestResponsesEncodeRequest_FromIR(t *testing.T) {
	req := ir.Request{
		Model:     "gpt-5",
		MaxTokens: 512,
		System:    []ir.TextPart{{Text: "You are helpful."}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "Hi"}}},
		},
		Tools: []ir.Tool{{Name: "get_weather", InputSchema: []byte(`{"type":"object"}`)}},
	}
	raw, err := oa.ResponsesCodec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	got, err := oa.ResponsesCodec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest(round trip): %v", err)
	}
	if got.Model != req.Model || got.MaxTokens != 512 {
		t.Errorf("round trip model/max_output_tokens = %q/%d", got.Model, got.MaxTokens)
	}
	if len(got.System) != 1 || got.System[0].Text != "You are helpful." {
		t.Errorf("round trip instructions = %+v", got.System)
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != ir.RoleUser {
		t.Errorf("round trip messages = %+v", got.Messages)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "get_weather" {
		t.Errorf("round trip tools = %+v", got.Tools)
	}
}

func TestResponsesEncodeResponse_FromIR(t *testing.T) {
	resp := ir.Response{
		ID:    "resp_test",
		Model: "gpt-5",
		Content: []ir.Part{
			ir.TextPart{Text: "Hello"},
			ir.ToolCallPart{ID: "call_1", Name: "get_weather", Input: []byte(`{"location":"SF"}`)},
		},
		StopReason: ir.StopToolUse,
		Usage:      ir.Usage{InputTokens: 10, OutputTokens: 5},
	}
	raw, err := oa.ResponsesCodec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	got, err := oa.ResponsesCodec{}.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse(round trip): %v", err)
	}
	if got.ID != resp.ID || got.Model != resp.Model || got.StopReason != ir.StopToolUse {
		t.Errorf("round trip id/model/stop_reason: %+v", got)
	}
	if len(got.Content) != 2 {
		t.Fatalf("round trip content = %d, want 2", len(got.Content))
	}
	if txt, ok := got.Content[0].(ir.TextPart); !ok || txt.Text != "Hello" {
		t.Errorf("round trip text = %+v", got.Content[0])
	}
	if tc, ok := got.Content[1].(ir.ToolCallPart); !ok || tc.ID != "call_1" || tc.Name != "get_weather" || string(tc.Input) != `{"location":"SF"}` {
		t.Errorf("round trip tool_call = %+v", got.Content[1])
	}
	if got.Usage.InputTokens != 10 || got.Usage.OutputTokens != 5 {
		t.Errorf("round trip usage = %+v", got.Usage)
	}
}
