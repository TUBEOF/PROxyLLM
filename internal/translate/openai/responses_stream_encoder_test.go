package openai_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/proxyllm/proxyllm/internal/translate/ir"
	oa "github.com/proxyllm/proxyllm/internal/translate/openai"
	"github.com/proxyllm/proxyllm/internal/translate/sse"
	"github.com/proxyllm/proxyllm/internal/translate/testutil"
)

// Responses-API stream-encoder contract tests, RED-GREEN per Analysis F.
// The encoder must produce the Responses-API named-SSE wire format (facts O5,
// O7; developers.openai.com/api/docs/api-reference/responses-streaming,
// accessed 2026-09-10) such that the package's own fixture-verified stream
// decoder reconstructs the canonical events (round-trip discipline).
//
// Spec-derived wire assertions follow the golden fixture
// testdata/golden/openai/responses-stream.sse. The fixture carries the FULL
// response snapshot on response.created/response.in_progress/response.completed,
// but the canonical IR only preserves id+model+usage — the encoder mints a
// minimal-but-conformant snapshot, so the round trip compares event-by-event
// (named events + canonical semantics), not raw bytes. The exact named-event
// shapes (event: response.created, event: response.output_text.delta, etc.)
// ARE asserted verbatim.

// encodeResponsesAll runs evs through a fresh Responses encoder over a buffer
// and returns the wire bytes.
func encodeResponsesAll(t *testing.T, evs []ir.StreamEvent) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := oa.NewResponsesStreamEncoder(&buf)
	for _, ev := range evs {
		if err := enc.Write(context.Background(), ev); err != nil {
			t.Fatalf("Write(%s): %v", ev.Type, err)
		}
	}
	return buf.Bytes()
}

// parseSSEEvents parses an SSE byte stream into a list of (name, data) pairs
// for named events, skipping comments. Used to compare wire shapes
// event-by-event rather than raw bytes (the encoder's response snapshot is
// minimal — the IR does not carry instructions/temperature/etc.).
func parseSSEEvents(t *testing.T, raw []byte) []sseNamedEvent {
	t.Helper()
	r := sse.NewReader(bytes.NewReader(raw))
	var out []sseNamedEvent
	for {
		ev, err := r.Next()
		if err != nil {
			break
		}
		if ev.IsComment {
			continue
		}
		out = append(out, sseNamedEvent{Name: ev.Name, Data: ev.Data})
	}
	return out
}

type sseNamedEvent struct {
	Name string
	Data string
}

// sseDataResponse is a minimal shape to pull .type and .response.status out
// of a named event's data payload.
type sseDataResponse struct {
	Type     string `json:"type"`
	Response *struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Model  string `json:"model"`
		Usage  *struct {
			InputTokens         int `json:"input_tokens"`
			OutputTokens        int `json:"output_tokens"`
			TotalTokens         int `json:"total_tokens"`
			OutputTokensDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details,omitempty"`
		} `json:"usage,omitempty"`
	} `json:"response,omitempty"`
	OutputIndex int `json:"output_index,omitempty"`
	Item        *struct {
		Type   string `json:"type"`
		ID     string `json:"id"`
		CallID string `json:"call_id"`
		Name   string `json:"name"`
	} `json:"item,omitempty"`
	Delta string `json:"delta,omitempty"`
}

// TestResponsesStreamEncode_FullSequenceRoundTrip: decode the golden fixture,
// re-encode the canonical events, and assert the output round-trips back
// through the decoder to the same canonical sequence. Also assert the exact
// named-event types are present in the encoded wire.
func TestResponsesStreamEncode_FullSequenceRoundTrip(t *testing.T) {
	wire := testutil.LoadGolden(t, "openai/responses-stream.sse")
	want := testutil.LoadExpectedEvents(t, "openai/responses-stream.expected.json")

	// Decode the golden fixture → canonical events.
	decoded := testutil.DrainStreamDecoder(t, oa.NewResponsesStreamDecoder(bytes.NewReader(wire)))

	// The decoded events must match the golden expected sequence (sanity:
	// the decoder is fixture-verified; this proves our input is correct).
	testutil.AssertEventsEqual(t, "openai/responses-stream.sse", want, decoded)

	// Re-encode the canonical events → wire.
	encoded := encodeResponsesAll(t, decoded)

	// Assert the exact named-event types appear (O7: named SSE events).
	events := parseSSEEvents(t, encoded)
	wantNames := []string{
		"response.created",
		"response.output_item.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_item.done",
		"response.completed",
	}
	if len(events) != len(wantNames) {
		t.Fatalf("encoded event count = %d, want %d; events=%+v", len(events), len(wantNames), events)
	}
	for i, want := range wantNames {
		if events[i].Name != want {
			t.Errorf("encoded event %d: name = %q, want %q", i, events[i].Name, want)
		}
	}

	// Round trip: the fixture-verified decoder must reconstruct the exact
	// canonical sequence from the encoded wire.
	got := testutil.DrainStreamDecoder(t, oa.NewResponsesStreamDecoder(bytes.NewReader(encoded)))
	testutil.AssertEventsEqual(t, "responses-stream-encoder round-trip", want, got)
}

// TestResponsesStreamEncode_CreatedSnapshotShape: response.created carries a
// full response snapshot with id, object:"response", status:"in_progress",
// model, and usage:null (fixture line 2).
func TestResponsesStreamEncode_CreatedSnapshotShape(t *testing.T) {
	evs := []ir.StreamEvent{
		{Type: ir.Start, Response: &ir.Response{
			ID:    "resp_abc123",
			Model: "gpt-5",
		}},
	}
	encoded := encodeResponsesAll(t, evs)
	events := parseSSEEvents(t, encoded)
	if len(events) != 1 || events[0].Name != "response.created" {
		t.Fatalf("events = %+v, want one response.created", events)
	}
	var d sseDataResponse
	if err := json.Unmarshal([]byte(events[0].Data), &d); err != nil {
		t.Fatalf("unmarshal response.created data: %v", err)
	}
	if d.Type != "response.created" {
		t.Errorf("type = %q, want response.created", d.Type)
	}
	if d.Response == nil {
		t.Fatal("response snapshot missing")
	}
	if d.Response.ID != "resp_abc123" {
		t.Errorf("response.id = %q, want resp_abc123", d.Response.ID)
	}
	if d.Response.Model != "gpt-5" {
		t.Errorf("response.model = %q, want gpt-5", d.Response.Model)
	}
	if d.Response.Status != "in_progress" {
		t.Errorf("response.status = %q, want in_progress", d.Response.Status)
	}
	// usage must be JSON null on response.created (fixture line 2).
	if !strings.Contains(events[0].Data, `"usage":null`) {
		t.Errorf("response.created must carry usage:null, got: %s", events[0].Data)
	}
	// object must be "response" (fixture line 2).
	if !strings.Contains(events[0].Data, `"object":"response"`) {
		t.Errorf("response.created must carry object:\"response\", got: %s", events[0].Data)
	}
}

// TestResponsesStreamEncode_MessageItemShape: BlockStart with ir.TextPart →
// response.output_item.added with {item:{id:"msg_...", type:"message",
// status:"in_progress", role:"assistant", content:[]}} (fixture line 8).
func TestResponsesStreamEncode_MessageItemShape(t *testing.T) {
	evs := []ir.StreamEvent{
		{Type: ir.Start, Response: &ir.Response{ID: "resp_1", Model: "gpt-5"}},
		{Type: ir.BlockStart, Index: 0, Part: ir.TextPart{}},
	}
	encoded := encodeResponsesAll(t, evs)
	events := parseSSEEvents(t, encoded)

	// Find the output_item.added event.
	var added *sseNamedEvent
	for i := range events {
		if events[i].Name == "response.output_item.added" {
			added = &events[i]
			break
		}
	}
	if added == nil {
		t.Fatalf("no response.output_item.added in %v", events)
	}
	var d sseDataResponse
	if err := json.Unmarshal([]byte(added.Data), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.OutputIndex != 0 {
		t.Errorf("output_index = %d, want 0", d.OutputIndex)
	}
	if d.Item == nil {
		t.Fatal("item missing")
	}
	if d.Item.Type != "message" {
		t.Errorf("item.type = %q, want message", d.Item.Type)
	}
	if d.Item.ID == "" || !strings.HasPrefix(d.Item.ID, "msg_") {
		t.Errorf("item.id = %q, want msg_ prefix", d.Item.ID)
	}
	// content must be an empty array (fixture line 8).
	if !strings.Contains(added.Data, `"content":[]`) {
		t.Errorf("item.content must be [], got: %s", added.Data)
	}
	if !strings.Contains(added.Data, `"status":"in_progress"`) {
		t.Errorf("item.status must be in_progress, got: %s", added.Data)
	}
	if !strings.Contains(added.Data, `"role":"assistant"`) {
		t.Errorf("item.role must be assistant, got: %s", added.Data)
	}
}

// TestResponsesStreamEncode_TextDeltaShape: Delta(text) →
// response.output_text.delta with {item_id, output_index, content_index,
// delta} (fixture line 14).
func TestResponsesStreamEncode_TextDeltaShape(t *testing.T) {
	evs := []ir.StreamEvent{
		{Type: ir.Start, Response: &ir.Response{ID: "resp_1", Model: "gpt-5"}},
		{Type: ir.BlockStart, Index: 0, Part: ir.TextPart{}},
		{Type: ir.Delta, Index: 0, Delta: ir.DeltaPayload{Kind: ir.DeltaText, Text: "Hi"}},
	}
	encoded := encodeResponsesAll(t, evs)
	events := parseSSEEvents(t, encoded)

	var delta *sseNamedEvent
	for i := range events {
		if events[i].Name == "response.output_text.delta" {
			delta = &events[i]
			break
		}
	}
	if delta == nil {
		t.Fatalf("no response.output_text.delta in %v", events)
	}
	var d sseDataResponse
	if err := json.Unmarshal([]byte(delta.Data), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Type != "response.output_text.delta" {
		t.Errorf("type = %q, want response.output_text.delta", d.Type)
	}
	if d.Delta != "Hi" {
		t.Errorf("delta = %q, want Hi", d.Delta)
	}
	if d.OutputIndex != 0 {
		t.Errorf("output_index = %d, want 0", d.OutputIndex)
	}
	if !strings.Contains(delta.Data, `"item_id":"`) {
		t.Errorf("item_id missing, got: %s", delta.Data)
	}
	if !strings.Contains(delta.Data, `"content_index":0`) {
		t.Errorf("content_index missing/wrong, got: %s", delta.Data)
	}
}

// TestResponsesStreamEncode_CompletedShape: MessageDelta →
// response.completed with status:"completed", usage mapped from IR, and
// output array mirroring the emitted message item (fixture line 29).
// Done is a no-op (the stream ends with response.completed).
func TestResponsesStreamEncode_CompletedShape(t *testing.T) {
	evs := []ir.StreamEvent{
		{Type: ir.Start, Response: &ir.Response{ID: "resp_1", Model: "gpt-5"}},
		{Type: ir.BlockStart, Index: 0, Part: ir.TextPart{}},
		{Type: ir.Delta, Index: 0, Delta: ir.DeltaPayload{Kind: ir.DeltaText, Text: "Hi"}},
		{Type: ir.BlockStop, Index: 0},
		{Type: ir.MessageDelta, MessageDelta: &ir.MessageDeltaPayload{
			StopReason: ir.StopEndTurn,
			Usage:      ir.Usage{InputTokens: 37, OutputTokens: 11, ReasoningTokens: 0},
		}},
		{Type: ir.Done},
	}
	encoded := encodeResponsesAll(t, evs)
	events := parseSSEEvents(t, encoded)

	// The last event must be response.completed (not a separate done event).
	if len(events) == 0 {
		t.Fatal("no events emitted")
	}
	last := events[len(events)-1]
	if last.Name != "response.completed" {
		t.Fatalf("last event = %q, want response.completed", last.Name)
	}
	var d sseDataResponse
	if err := json.Unmarshal([]byte(last.Data), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Type != "response.completed" {
		t.Errorf("type = %q, want response.completed", d.Type)
	}
	if d.Response == nil {
		t.Fatal("response snapshot missing")
	}
	if d.Response.Status != "completed" {
		t.Errorf("status = %q, want completed", d.Response.Status)
	}
	if d.Response.Usage == nil {
		t.Fatal("usage missing on response.completed")
	}
	if d.Response.Usage.InputTokens != 37 || d.Response.Usage.OutputTokens != 11 {
		t.Errorf("usage in/out = %d/%d, want 37/11", d.Response.Usage.InputTokens, d.Response.Usage.OutputTokens)
	}
	if d.Response.Usage.TotalTokens != 48 {
		t.Errorf("total_tokens = %d, want 48", d.Response.Usage.TotalTokens)
	}
	if d.Response.Usage.OutputTokensDetails == nil ||
		d.Response.Usage.OutputTokensDetails.ReasoningTokens != 0 {
		t.Errorf("output_tokens_details.reasoning_tokens wrong, got: %+v", d.Response.Usage.OutputTokensDetails)
	}
	// output array must contain the completed message item (fixture line 29).
	if !strings.Contains(last.Data, `"output":[{`) {
		t.Errorf("response.completed must carry output array with items, got: %s", last.Data)
	}
	if !strings.Contains(last.Data, `"type":"message"`) {
		t.Errorf("output must contain a message item, got: %s", last.Data)
	}
	if !strings.Contains(last.Data, `"status":"completed"`) {
		t.Errorf("output item must have status completed, got: %s", last.Data)
	}
}

// ---- mappings NOT covered by the golden fixture (meta.json note) ----

// TestResponsesStreamEncode_FunctionCallItem: BlockStart with
// ir.ToolCallPart → response.output_item.added with
// {type:"function_call", call_id, name} (O7); Delta(tool_input) →
// response.function_call_arguments.delta (O7).
func TestResponsesStreamEncode_FunctionCallItem(t *testing.T) {
	evs := []ir.StreamEvent{
		{Type: ir.Start, Response: &ir.Response{ID: "resp_1", Model: "gpt-5"}},
		{Type: ir.BlockStart, Index: 0, Part: ir.ToolCallPart{
			ID:   "call_1",
			Name: "get_weather",
		}},
		{Type: ir.Delta, Index: 0, Delta: ir.DeltaPayload{
			Kind:        ir.DeltaToolInput,
			PartialJSON: `{"location":"SF"}`,
		}},
		{Type: ir.BlockStop, Index: 0},
		{Type: ir.MessageDelta, MessageDelta: &ir.MessageDeltaPayload{
			StopReason: ir.StopToolUse,
			Usage:      ir.Usage{InputTokens: 10, OutputTokens: 3},
		}},
		{Type: ir.Done},
	}
	encoded := encodeResponsesAll(t, evs)
	events := parseSSEEvents(t, encoded)

	// response.output_item.added must carry a function_call item.
	var added *sseNamedEvent
	for i := range events {
		if events[i].Name == "response.output_item.added" {
			added = &events[i]
			break
		}
	}
	if added == nil {
		t.Fatalf("no response.output_item.added in %v", events)
	}
	if !strings.Contains(added.Data, `"type":"function_call"`) {
		t.Errorf("item.type must be function_call, got: %s", added.Data)
	}
	if !strings.Contains(added.Data, `"call_id":"call_1"`) {
		t.Errorf("item.call_id must be call_1, got: %s", added.Data)
	}
	if !strings.Contains(added.Data, `"name":"get_weather"`) {
		t.Errorf("item.name must be get_weather, got: %s", added.Data)
	}

	// response.function_call_arguments.delta must carry the fragment.
	var argDelta *sseNamedEvent
	for i := range events {
		if events[i].Name == "response.function_call_arguments.delta" {
			argDelta = &events[i]
			break
		}
	}
	if argDelta == nil {
		t.Fatalf("no response.function_call_arguments.delta in %v", events)
	}
	var d sseDataResponse
	if err := json.Unmarshal([]byte(argDelta.Data), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Delta != `{"location":"SF"}` {
		t.Errorf("delta = %q, want {\"location\":\"SF\"}", d.Delta)
	}

	// Round trip through the decoder.
	got := testutil.DrainStreamDecoder(t, oa.NewResponsesStreamDecoder(bytes.NewReader(encoded)))
	if len(got) < 4 {
		t.Fatalf("round trip produced %d events, want >= 4", len(got))
	}
	if got[1].Type != ir.BlockStart {
		t.Fatalf("event 1 = %s, want block_start", got[1].Type)
	}
	tc, ok := got[1].Part.(ir.ToolCallPart)
	if !ok {
		t.Fatalf("event 1 part = %T, want ir.ToolCallPart", got[1].Part)
	}
	if tc.ID != "call_1" || tc.Name != "get_weather" {
		t.Errorf("tool_use id/name = %q/%q, want call_1/get_weather", tc.ID, tc.Name)
	}
	if got[2].Type != ir.Delta || got[2].Delta.Kind != ir.DeltaToolInput ||
		got[2].Delta.PartialJSON != `{"location":"SF"}` {
		t.Errorf("event 2 delta = %+v, want tool_input {\"location\":\"SF\"}", got[2].Delta)
	}
}

// TestResponsesStreamEncode_FunctionCallArgumentsAccumulate (RED-GREEN):
// fragmented tool-argument deltas must reassemble in the
// response.completed snapshot. Spec: the Responses completed snapshot
// carries the function_call item with the FULL assembled arguments
// string (fixture testdata/golden/openai/responses-stream.sse documents
// completed snapshots as full state; OpenAI stream function-calls
// fragment arguments across many function_call_arguments.delta events).
// The encoder must accumulate DeltaToolInput fragments just as it does
// DeltaText, or every completed function_call ships arguments:"" — and a
// Responses client re-issuing the tool call from the snapshot calls the
// tool with no arguments at all.
func TestResponsesStreamEncode_FunctionCallArgumentsAccumulate(t *testing.T) {
	evs := []ir.StreamEvent{
		{Type: ir.Start, Response: &ir.Response{ID: "resp_1", Model: "gpt-5"}},
		{Type: ir.BlockStart, Index: 0, Part: ir.ToolCallPart{
			ID:   "call_1",
			Name: "get_weather",
		}},
		// Arguments arrive fragmented, as they do on the wire.
		{Type: ir.Delta, Index: 0, Delta: ir.DeltaPayload{
			Kind:        ir.DeltaToolInput,
			PartialJSON: `{"loc`,
		}},
		{Type: ir.Delta, Index: 0, Delta: ir.DeltaPayload{
			Kind:        ir.DeltaToolInput,
			PartialJSON: `ation":"SF"}`,
		}},
		{Type: ir.BlockStop, Index: 0},
		{Type: ir.MessageDelta, MessageDelta: &ir.MessageDeltaPayload{
			StopReason: ir.StopToolUse,
			Usage:      ir.Usage{InputTokens: 10, OutputTokens: 3},
		}},
		{Type: ir.Done},
	}
	encoded := encodeResponsesAll(t, evs)
	events := parseSSEEvents(t, encoded)

	var completed *sseNamedEvent
	for i := range events {
		if events[i].Name == "response.completed" {
			completed = &events[i]
			break
		}
	}
	if completed == nil {
		t.Fatalf("no response.completed in %v", events)
	}
	want := `{"location":"SF"}`
	var snap struct {
		Response struct {
			Output []struct {
				Type      string `json:"type"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"output"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(completed.Data), &snap); err != nil {
		t.Fatalf("decode completed snapshot: %v", err)
	}
	if len(snap.Response.Output) != 1 {
		t.Fatalf("completed snapshot must carry exactly one output item, got %d: %s", len(snap.Response.Output), completed.Data)
	}
	if snap.Response.Output[0].Type != "function_call" || snap.Response.Output[0].CallID != "call_1" || snap.Response.Output[0].Name != "get_weather" {
		t.Errorf("unexpected function_call item: %+v", snap.Response.Output[0])
	}
	if snap.Response.Output[0].Arguments != want {
		t.Errorf("completed snapshot must carry assembled arguments %q, got %q", want, snap.Response.Output[0].Arguments)
	}
}

// TestResponsesStreamEncode_ReasoningItem: BlockStart with ir.ThinkingPart →
// response.output_item.added with type:"reasoning" (O7).
func TestResponsesStreamEncode_ReasoningItem(t *testing.T) {
	evs := []ir.StreamEvent{
		{Type: ir.Start, Response: &ir.Response{ID: "resp_1", Model: "gpt-5"}},
		{Type: ir.BlockStart, Index: 0, Part: ir.ThinkingPart{}},
		{Type: ir.Delta, Index: 0, Delta: ir.DeltaPayload{Kind: ir.DeltaThinking, Text: "hmm"}},
		{Type: ir.BlockStop, Index: 0},
		{Type: ir.MessageDelta, MessageDelta: &ir.MessageDeltaPayload{
			StopReason: ir.StopEndTurn,
			Usage:      ir.Usage{InputTokens: 5, OutputTokens: 2},
		}},
		{Type: ir.Done},
	}
	encoded := encodeResponsesAll(t, evs)
	events := parseSSEEvents(t, encoded)

	var added *sseNamedEvent
	for i := range events {
		if events[i].Name == "response.output_item.added" {
			added = &events[i]
			break
		}
	}
	if added == nil {
		t.Fatalf("no response.output_item.added in %v", events)
	}
	if !strings.Contains(added.Data, `"type":"reasoning"`) {
		t.Errorf("item.type must be reasoning, got: %s", added.Data)
	}

	// reasoning_summary_text.delta must carry the thinking fragment.
	var sumDelta *sseNamedEvent
	for i := range events {
		if events[i].Name == "response.reasoning_summary_text.delta" {
			sumDelta = &events[i]
			break
		}
	}
	if sumDelta == nil {
		t.Fatalf("no response.reasoning_summary_text.delta in %v", events)
	}
	var d sseDataResponse
	if err := json.Unmarshal([]byte(sumDelta.Data), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Delta != "hmm" {
		t.Errorf("delta = %q, want hmm", d.Delta)
	}
}

// TestResponsesStreamEncode_ErrorEvent: terminal ir.Error →
// event: error with FLAT {code, message, param} shape (O7 — Responses error
// events are FLAT, unlike chat).
func TestResponsesStreamEncode_ErrorEvent(t *testing.T) {
	evs := []ir.StreamEvent{
		{Type: ir.Start, Response: &ir.Response{ID: "resp_1", Model: "gpt-5"}},
		{Type: ir.Error, Error: &ir.UpstreamError{
			Code:    "server_error",
			Message: "The server had an error.",
		}},
	}
	encoded := encodeResponsesAll(t, evs)
	events := parseSSEEvents(t, encoded)

	var errEvent *sseNamedEvent
	for i := range events {
		if events[i].Name == "error" {
			errEvent = &events[i]
			break
		}
	}
	if errEvent == nil {
		t.Fatalf("no error event in %v", events)
	}
	// FLAT shape: {type, code, message, param} at top level (O7).
	if !strings.Contains(errEvent.Data, `"type":"error"`) {
		t.Errorf("error event must carry type:\"error\", got: %s", errEvent.Data)
	}
	if !strings.Contains(errEvent.Data, `"code":"server_error"`) {
		t.Errorf("error event must carry code:\"server_error\", got: %s", errEvent.Data)
	}
	if !strings.Contains(errEvent.Data, `"message":"The server had an error."`) {
		t.Errorf("error event must carry message, got: %s", errEvent.Data)
	}
	// Must NOT have a nested error object (unlike chat wire).
	if strings.Contains(errEvent.Data, `"error":{`) {
		t.Errorf("error event must be FLAT (no nested error object), got: %s", errEvent.Data)
	}

	// Round trip: decoder reconstructs the terminal error.
	got := testutil.DrainStreamDecoder(t, oa.NewResponsesStreamDecoder(bytes.NewReader(encoded)))
	if len(got) != 2 || got[1].Type != ir.Error {
		t.Fatalf("round trip = %d events, want start + error", len(got))
	}
	if got[1].Error == nil || got[1].Error.Code != "server_error" ||
		got[1].Error.Message != "The server had an error." {
		t.Errorf("round trip error = %+v", got[1].Error)
	}
}

// TestResponsesStreamEncode_UnknownKindComment: unknown event kind → comment
// line (A5 discipline: never fail the stream on unknown kinds).
func TestResponsesStreamEncode_UnknownKindComment(t *testing.T) {
	evs := []ir.StreamEvent{
		{Type: ir.StreamEventType("bogus_kind")},
	}
	encoded := encodeResponsesAll(t, evs)
	// Must produce a comment line (starts with ':').
	if !bytes.HasPrefix(encoded, []byte(":")) {
		t.Errorf("unknown kind must relay as a comment line, got: %q", encoded)
	}
}

// TestResponsesStreamEncode_PingComment: Ping/Comment → comment line (A5).
func TestResponsesStreamEncode_PingComment(t *testing.T) {
	for _, typ := range []ir.StreamEventType{ir.Ping, ir.Comment} {
		evs := []ir.StreamEvent{{Type: typ}}
		encoded := encodeResponsesAll(t, evs)
		if !bytes.HasPrefix(encoded, []byte(":")) {
			t.Errorf("%s must relay as a comment line, got: %q", typ, encoded)
		}
	}
}
