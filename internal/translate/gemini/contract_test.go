package gemini_test

// Contract tests for the Gemini v1beta codec, RED-GREEN. Every assertion
// derives from the golden fixtures in testdata/golden/gemini/ (official API
// reference + v1beta discovery doc, accessed 2026-11-03).

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	gm "github.com/tubeof/proxyllm/internal/translate/gemini"
	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/testutil"
)

func TestDecodeRequest_GenerateRequest(t *testing.T) {
	raw := testutil.LoadGolden(t, "gemini/generate-request.json")
	req, err := gm.Codec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}

	if req.Model != "" {
		t.Errorf("model = %q, want empty (the model rides the URL path)", req.Model)
	}
	if len(req.System) != 1 || req.System[0].Text != "You are a helpful assistant." {
		t.Errorf("system = %+v", req.System)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(req.Messages))
	}
	// User turn: text + inline image.
	if txt, ok := req.Messages[0].Parts[0].(ir.TextPart); !ok || txt.Text != "What's the weather in San Francisco?" {
		t.Errorf("messages[0].parts[0] = %+v", req.Messages[0].Parts[0])
	}
	img, ok := req.Messages[0].Parts[1].(ir.ImagePart)
	if !ok || img.Source != "base64" || img.MediaType != "image/png" || img.Data != "aVBORw0KGgo=" {
		t.Errorf("messages[0].parts[1] = %+v", req.Messages[0].Parts[1])
	}
	// Assistant turn: thought part with its verbatim signature, then the
	// function call whose part carries the replay signature.
	if th, ok := req.Messages[1].Parts[0].(ir.ThinkingPart); !ok ||
		th.Thinking != "The user wants weather info. I should call get_weather." ||
		th.Signature != "c2lnX3RoaW5raW5n" {
		t.Errorf("messages[1].parts[0] = %+v", req.Messages[1].Parts[0])
	}
	tc, ok := req.Messages[1].Parts[1].(ir.ToolCallPart)
	if !ok || tc.ID != "call_1" || tc.Name != "get_weather" ||
		!strings.Contains(string(tc.Input), `"location"`) {
		t.Errorf("messages[1].parts[1] = %+v", req.Messages[1].Parts[1])
	}
	// Function response answers the matching id (raw object bytes ride the
	// IR text part verbatim).
	tr, ok := req.Messages[2].Parts[0].(ir.ToolResultPart)
	if !ok || tr.ToolUseID != "call_1" || tr.IsError {
		t.Fatalf("messages[2].parts[0] = %+v", req.Messages[2].Parts[0])
	}
	if tp, ok := tr.Content[0].(ir.TextPart); !ok || !strings.Contains(tp.Text, `"temperature"`) {
		t.Errorf("function response body = %+v", tr.Content[0])
	}
	// Tools decode from functionDeclarations.
	if len(req.Tools) != 1 || req.Tools[0].Name != "get_weather" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	if tc := req.ToolChoice; tc == nil || tc.Type != ir.ToolChoiceAny {
		t.Errorf("tool choice = %+v, want any (ANY mode)", req.ToolChoice)
	}
	// generationConfig typed members.
	if req.MaxTokens != 1024 || req.Temperature == nil || *req.Temperature != 0.7 {
		t.Errorf("max_tokens/temperature = %d/%v", req.MaxTokens, req.Temperature)
	}
	if req.TopP == nil || *req.TopP != 0.9 || req.TopK == nil || *req.TopK != 40 {
		t.Errorf("topP/topK = %v/%v", req.TopP, req.TopK)
	}
	if strings.Join(req.StopSequences, ",") != "END" {
		t.Errorf("stop sequences = %v", req.StopSequences)
	}
	if req.Thinking == nil || !req.Thinking.Enabled || req.Thinking.BudgetTokens != 512 {
		t.Errorf("thinking = %+v", req.Thinking)
	}
	if req.ReasoningEffort != "medium" {
		t.Errorf("reasoning effort = %q, want medium (thinkingLevel MEDIUM)", req.ReasoningEffort)
	}
	if string(req.ResponseFormat) != `{"type":"json_object"}` {
		t.Errorf("response format = %s", req.ResponseFormat)
	}
	// Top-level unknowns ride passthrough; nested generationConfig unknowns
	// ride codec extras ("seed" is a documented member the codec does not
	// type).
	if req.Passthrough["labels"] == nil {
		t.Errorf("labels missing from passthrough: %v", keys(req.Passthrough))
	}
}

func keys(m map[string]json.RawMessage) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The decoded request must re-encode to a body the same codec decodes back
// with equal key state — and the thought signatures must ride verbatim.
func TestEncodeRequest_RoundtripKeepsSignaturesAndUnknowns(t *testing.T) {
	raw := testutil.LoadGolden(t, "gemini/generate-request.json")
	req, err := gm.Codec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	out, err := gm.Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("decode re-encoded body: %v", err)
	}

	// The body never carries a top-level model field (the path governs) —
	// role:"model" strings must not trip the check.
	for key := range body {
		if key == "model" {
			t.Errorf("body must not carry a model field:\n%s", out)
		}
	}
	// Function-call part: the replay signature rides THAT part (order of
	// the members is JSON-neutral).
	var contents []wireContentProbe
	if err := json.Unmarshal(body["contents"], &contents); err != nil {
		t.Fatalf("decode contents: %v", err)
	}
	if len(contents) != 3 {
		t.Fatalf("re-encoded contents = %d turns", len(contents))
	}
	fcPart := contents[1].Parts[1]
	if fcPart.FunctionCall == nil || fcPart.FunctionCall.ID != "call_1" ||
		fcPart.ThoughtSignature != "c2ln" {
		t.Errorf("functionCall part = %+v", fcPart)
	}
	// Thought part replays as thought:true with its own signature.
	thPart := contents[1].Parts[0]
	if !thPart.Thought || thPart.Text != "The user wants weather info. I should call get_weather." ||
		thPart.ThoughtSignature != "c2lnX3RoaW5raW5n" {
		t.Errorf("thought part = %+v", thPart)
	}
	// Tool choice maps to ANY with the fixture's allowed names.
	var tcProbe struct {
		FunctionCallingConfig struct {
			Mode string `json:"mode"`
		} `json:"functionCallingConfig"`
	}
	if err := json.Unmarshal(body["toolConfig"], &tcProbe); err != nil {
		t.Fatalf("decode toolConfig: %v", err)
	}
	if tcProbe.FunctionCallingConfig.Mode != "ANY" {
		t.Errorf("toolConfig mode = %q", tcProbe.FunctionCallingConfig.Mode)
	}
	// Top-level passthrough (labels) is re-emitted.
	if body["labels"] == nil {
		t.Errorf("passthrough labels not re-emitted:\n%s", out)
	}
	// Nested generationConfig extras (seed) survive same-protocol re-encode.
	if !strings.Contains(string(body["generationConfig"]), `"seed":7`) {
		t.Errorf("generationConfig seed not re-emitted:\n%s", out)
	}

	// And decode again: key state is equal.
	got, err := gm.Codec{}.DecodeRequest(out)
	if err != nil {
		t.Fatalf("DecodeRequest(round trip): %v", err)
	}
	if len(got.Messages) != len(req.Messages) {
		t.Fatalf("round trip messages = %d, want %d", len(got.Messages), len(req.Messages))
	}
	if len(got.System) != 1 || got.System[0].Text != "You are a helpful assistant." {
		t.Errorf("round trip system = %+v", got.System)
	}
	gotTool, ok := got.Messages[2].Parts[0].(ir.ToolResultPart)
	if !ok || gotTool.ToolUseID != "call_1" {
		t.Errorf("round trip tool result = %+v", got.Messages[2].Parts)
	}
}

// wireContentProbe mirrors one re-encoded contents turn for assertions.
type wireContentProbe struct {
	Role  string          `json:"role"`
	Parts []wirePartProbe `json:"parts"`
}

type wirePartProbe struct {
	Text             string `json:"text,omitempty"`
	Thought          bool   `json:"thought,omitempty"`
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
	FunctionCall     *struct {
		ID string `json:"id"`
	} `json:"functionCall,omitempty"`
}

func TestEncodeRequest_ThinkingLevelAndIncludeThoughts(t *testing.T) {
	req := ir.Request{
		Messages:        []ir.Message{{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "Hi"}}}},
		Thinking:        &ir.ThinkingConfig{Enabled: true, BudgetTokens: 9000},
		ReasoningEffort: "high",
	}
	out, err := gm.Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `"includeThoughts":true`) {
		t.Errorf("includeThoughts missing:\n%s", out)
	}
	if !strings.Contains(string(out), `"thinkingLevel":"HIGH"`) {
		t.Errorf("thinkingLevel HIGH missing:\n%s", out)
	}
}

// Mode NONE is the tool-refusal switch; tool (a named call) maps to ANY with
// allowedFunctionNames.
func TestEncodeRequest_ToolChoiceModes(t *testing.T) {
	none, err := gm.Codec{}.EncodeRequest(ir.Request{
		Messages:   []ir.Message{{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "Hi"}}}},
		ToolChoice: &ir.ToolChoice{Type: ir.ToolChoiceNone},
	})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(none), `"mode":"NONE"`) {
		t.Errorf("mode NONE missing: %s", none)
	}
	pinned, err := gm.Codec{}.EncodeRequest(ir.Request{
		Messages:   []ir.Message{{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "Hi"}}}},
		Tools:      []ir.Tool{{Name: "get_weather"}},
		ToolChoice: &ir.ToolChoice{Type: ir.ToolChoiceTool, Name: "get_weather"},
	})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(pinned), `"allowedFunctionNames":["get_weather"]`) {
		t.Errorf("allowedFunctionNames missing: %s", pinned)
	}
}

// The function response must name the function it answers: the name comes
// from the matched earlier tool call (by id).
func TestEncodeRequest_ToolResultNameResolution(t *testing.T) {
	req := ir.Request{
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, Parts: []ir.Part{ir.ToolCallPart{ID: "call_7", Name: "lookup", Input: []byte(`{"q":"x"}`)}}},
			{Role: ir.RoleTool, Parts: []ir.Part{ir.ToolResultPart{
				ToolUseID: "call_7",
				Content:   []ir.Part{ir.TextPart{Text: `{"n":1}`}},
			}}},
		},
	}
	out, err := gm.Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `{"id":"call_7","name":"lookup","response":{"n":1}}`) {
		t.Errorf("functionResponse name/response wrong:\n%s", out)
	}
	// And back: the response round trips verbatim.
	got, err := gm.Codec{}.DecodeRequest(out)
	if err != nil {
		t.Fatalf("DecodeRequest(round trip): %v", err)
	}
	tr := got.Messages[1].Parts[0].(ir.ToolResultPart)
	if tp, ok := tr.Content[0].(ir.TextPart); !ok || !strings.Contains(tp.Text, `"n":1`) {
		t.Errorf("round trip tool result = %+v", tr.Content)
	}
}

func TestDecodeResponse_GenerateResponse(t *testing.T) {
	raw := testutil.LoadGolden(t, "gemini/generate-response.json")
	resp, err := gm.Codec{}.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.ID != "resp-request-123" || resp.Model != "gemini-2.5-flash" {
		t.Errorf("id/model = %q/%q", resp.ID, resp.Model)
	}
	// STOP + functionCall parts read as tool_use.
	if resp.StopReason != ir.StopToolUse {
		t.Errorf("stop reason = %q, want tool_use", resp.StopReason)
	}
	if len(resp.Content) != 4 {
		t.Fatalf("content = %d parts, want 4", len(resp.Content))
	}
	if th, ok := resp.Content[0].(ir.ThinkingPart); !ok || th.Signature == "" {
		t.Errorf("content[0] = %+v", resp.Content[0])
	}
	if sig, ok := resp.Content[2].(ir.ThinkingPart); !ok || sig.Thinking != "" || sig.Signature != "c2ln" {
		t.Errorf("content[2] = %+v, want bare signature thinking part", resp.Content[2])
	}
	if tc, ok := resp.Content[3].(ir.ToolCallPart); !ok || tc.ID != "call_1" || tc.Name != "get_weather" {
		t.Errorf("content[3] = %+v", resp.Content[3])
	}
	// Usage maps incl. thoughtsTokenCount → reasoning_tokens.
	if resp.Usage.InputTokens != 26 || resp.Usage.OutputTokens != 17 ||
		resp.Usage.ReasoningTokens != 12 || resp.Usage.CacheReadInputTokens != 8 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestDecodeResponse_BlockedPrompt(t *testing.T) {
	raw := testutil.LoadGolden(t, "gemini/generate-response-blocked.json")
	resp, err := gm.Codec{}.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.StopReason != ir.StopContentFilter {
		t.Errorf("stop reason = %q, want content_filter", resp.StopReason)
	}
	if len(resp.Content) != 0 {
		t.Errorf("content = %+v, want none", resp.Content)
	}
	if resp.Usage.InputTokens != 12 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestDecodeResponse_MalformedFunctionCall(t *testing.T) {
	raw := testutil.LoadGolden(t, "gemini/generate-response-malformed.json")
	resp, err := gm.Codec{}.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.StopReason != ir.StopRefusal {
		t.Errorf("stop reason = %q, want refusal", resp.StopReason)
	}
	if txt, ok := resp.Content[0].(ir.TextPart); !ok || txt.Text != "Let me call the" {
		t.Errorf("content = %+v", resp.Content)
	}
}

func TestEncodeResponse_Roundtrip(t *testing.T) {
	resp := ir.Response{
		ID:         "resp-x",
		Model:      "gemini-2.5-flash",
		StopReason: ir.StopToolUse,
		Content: []ir.Part{
			ir.ThinkingPart{Thinking: "Checking.", Signature: "c2lnTQ=="},
			ir.TextPart{Text: "Hello"},
			ir.ToolCallPart{ID: "call_1", Name: "get_weather", Input: []byte(`{"location":"SF"}`)},
		},
		Usage: ir.Usage{InputTokens: 10, OutputTokens: 5, ReasoningTokens: 3},
	}
	raw, err := gm.Codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(raw), `"finishReason":"STOP"`) {
		t.Errorf("the wire stops a tool-call turn with STOP, got %s", raw)
	}
	if !strings.Contains(string(raw), `"thoughtSignature":"c2lnTQ=="`) {
		t.Errorf("thinking signature must ride verbatim, got %s", raw)
	}
	got, err := gm.Codec{}.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse(round trip): %v", err)
	}
	if got.StopReason != ir.StopToolUse || got.Model != resp.Model ||
		len(got.Content) != 3 || got.Usage.InputTokens != 10 || got.Usage.ReasoningTokens != 3 {
		t.Errorf("round trip = %+v / %+v", got, got.Usage)
	}
}

func TestStreamDecode_FullSequence(t *testing.T) {
	wire := testutil.LoadGolden(t, "gemini/stream-generate.sse")
	want := testutil.LoadExpectedEvents(t, "gemini/stream-generate.sse.expected.json")

	got := testutil.DrainStreamDecoder(t, gm.NewStreamDecoder(bytes.NewReader(wire)))
	testutil.AssertEventsEqual(t, "gemini/stream-generate.sse", want, got)
}

func TestStreamDecode_BlockedStream(t *testing.T) {
	wire := testutil.LoadGolden(t, "gemini/stream-blocked.sse")
	want := testutil.LoadExpectedEvents(t, "gemini/stream-blocked.sse.expected.json")

	got := testutil.DrainStreamDecoder(t, gm.NewStreamDecoder(bytes.NewReader(wire)))
	testutil.AssertEventsEqual(t, "gemini/stream-blocked.sse", want, got)
}

// The stream encoder must produce the alt=sse wire such that the
// fixture-verified decoder reconstructs the canonical events (round-trip
// discipline; the decoder is pinned by the golden .sse fixture).
func TestStreamEncode_FullSequenceRoundTrip(t *testing.T) {
	rel := "gemini/stream-generate.sse.expected.json"
	want := testutil.LoadExpectedEvents(t, rel)

	var buf bytes.Buffer
	enc := gm.NewStreamEncoder(&buf)
	for _, ev := range want {
		if err := enc.Write(context.Background(), ev); err != nil {
			t.Fatalf("Write(%s): %v", ev.Type, err)
		}
	}
	got := testutil.DrainStreamDecoder(t, gm.NewStreamDecoder(bytes.NewReader(buf.Bytes())))
	testutil.AssertEventsEqual(t, rel, want, got)
}

// The countTokens body wraps the full request and never carries contents[]
// (the reference defines contents[] and generateContentRequest as mutually
// exclusive).
func TestCountTokens_RequestShape(t *testing.T) {
	body, err := gm.EncodeCountTokensRequest([]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	if err != nil {
		t.Fatalf("EncodeCountTokensRequest: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode wrapped body: %v", err)
	}
	if len(m) != 1 {
		t.Errorf("countTokens body keys = %v, want exactly generateContentRequest: %s", keys(m), body)
	}
	if _, ok := m["generateContentRequest"]; !ok {
		t.Errorf("generateContentRequest missing: %s", body)
	}
	total, err := gm.DecodeCountTokensResponse(testutil.LoadGolden(t, "gemini/count-tokens-response.json"))
	if err != nil {
		t.Fatalf("DecodeCountTokensResponse: %v", err)
	}
	if total != 342 {
		t.Errorf("totalTokens = %d, want 342", total)
	}
}
