package openai

// Request-field coverage for the cross-protocol controls added to the IR.
// Each test decodes a wire body, re-encodes it, and asserts the field made
// the round trip; the cross-protocol test asserts the response_format /
// text.format shape conversion.

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestChatRequestFieldsRoundTrip(t *testing.T) {
	raw := `{
		"model":"gpt-5",
		"messages":[{"role":"user","content":"hi"}],
		"stop":["END"],
		"response_format":{"type":"json_object"},
		"seed":7,
		"n":2,
		"parallel_tool_calls":false,
		"service_tier":"flex",
		"prompt_cache_key":"ck",
		"reasoning_effort":"high",
		"user":"u1",
		"custom_field":{"a":1}
	}`
	req, err := Codec{}.DecodeRequest([]byte(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got := decodeObject(t, out)
	assertJSONField(t, got, "stop", `["END"]`)
	assertJSONField(t, got, "response_format", `{"type":"json_object"}`)
	assertJSONField(t, got, "seed", `7`)
	if _, ok := got["n"]; ok {
		t.Errorf("n must not be forwarded: only choice 0 is relayed")
	}
	assertJSONField(t, got, "parallel_tool_calls", `false`)
	assertJSONField(t, got, "service_tier", `"flex"`)
	assertJSONField(t, got, "prompt_cache_key", `"ck"`)
	assertJSONField(t, got, "reasoning_effort", `"high"`)
	assertJSONField(t, got, "user", `"u1"`)
	assertJSONField(t, got, "custom_field", `{"a":1}`)
}

func TestResponsesRequestFieldsRoundTrip(t *testing.T) {
	raw := `{
		"model":"gpt-5",
		"input":"hi",
		"previous_response_id":"resp_1",
		"parallel_tool_calls":false,
		"service_tier":"flex",
		"include":["reasoning.encrypted_content"],
		"metadata":{"k":"v"},
		"client_metadata":{"c":"d"},
		"prompt_cache_key":"ck",
		"stream_options":{"include_usage":true},
		"text":{"format":{"type":"json_schema","name":"x","schema":{"type":"object"}},"verbosity":"low"},
		"reasoning":{"effort":"high","summary":"auto"},
		"store":false,
		"custom_field":1
	}`
	req, err := ResponsesCodec{}.DecodeRequest([]byte(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.PreviousResponseID != "resp_1" {
		t.Errorf("PreviousResponseID = %q", req.PreviousResponseID)
	}
	if req.ParallelToolCalls == nil || *req.ParallelToolCalls {
		t.Errorf("ParallelToolCalls = %v, want false", req.ParallelToolCalls)
	}
	if len(req.Include) != 1 || req.Include[0] != "reasoning.encrypted_content" {
		t.Errorf("Include = %v", req.Include)
	}
	if req.Metadata["k"] != "v" || req.ClientMetadata["c"] != "d" {
		t.Errorf("metadata = %v / %v", req.Metadata, req.ClientMetadata)
	}
	out, err := ResponsesCodec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got := decodeObject(t, out)
	assertJSONField(t, got, "previous_response_id", `"resp_1"`)
	assertJSONField(t, got, "parallel_tool_calls", `false`)
	assertJSONField(t, got, "service_tier", `"flex"`)
	assertJSONField(t, got, "include", `["reasoning.encrypted_content"]`)
	assertJSONField(t, got, "metadata", `{"k":"v"}`)
	assertJSONField(t, got, "client_metadata", `{"c":"d"}`)
	assertJSONField(t, got, "prompt_cache_key", `"ck"`)
	assertJSONField(t, got, "stream_options", `{"include_usage":true}`)
	assertJSONField(t, got, "text", `{"format":{"type":"json_schema","name":"x","schema":{"type":"object"}},"verbosity":"low"}`)
	assertJSONField(t, got, "reasoning", `{"effort":"high","summary":"auto"}`)
	assertJSONField(t, got, "store", `false`)
	assertJSONField(t, got, "custom_field", `1`)
}

func TestResponseFormatCrossProtocol(t *testing.T) {
	// Chat response_format → Responses text.format.
	chat := `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{"type":"object"}}}}`
	req, err := Codec{}.DecodeRequest([]byte(chat))
	if err != nil {
		t.Fatalf("decode chat: %v", err)
	}
	out, err := ResponsesCodec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode responses: %v", err)
	}
	got := decodeObject(t, out)
	assertJSONField(t, got, "text", `{"format":{"type":"json_schema","name":"x","schema":{"type":"object"}}}`)

	// Responses text.format → Chat response_format.
	resp := `{"model":"gpt-5","input":"hi","text":{"format":{"type":"json_schema","name":"x","schema":{"type":"object"}}}}`
	req, err = ResponsesCodec{}.DecodeRequest([]byte(resp))
	if err != nil {
		t.Fatalf("decode responses: %v", err)
	}
	out, err = Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode chat: %v", err)
	}
	got = decodeObject(t, out)
	assertJSONField(t, got, "response_format", `{"type":"json_schema","json_schema":{"name":"x","schema":{"type":"object"}}}`)
}

func TestChatRequest_StopString(t *testing.T) {
	req, err := Codec{}.DecodeRequest([]byte(
		`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"stop":"\n"}`,
	))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.StopSequences) != 1 || req.StopSequences[0] != "\n" {
		t.Fatalf("StopSequences = %q, want one newline", req.StopSequences)
	}
	out, err := Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	assertJSONField(t, decodeObject(t, out), "stop", `["\n"]`)
}

func TestResponsesBuiltinWebTools(t *testing.T) {
	raw := `{"model":"gpt-5","input":"hi","tools":[{"type":"web_search_preview"},{"type":"web_fetch"}]}`
	req, err := ResponsesCodec{}.DecodeRequest([]byte(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.Tools) != 2 {
		t.Fatalf("tools = %+v, want two server tools", req.Tools)
	}
	if req.Tools[0].ServerToolName() != "web_search" || req.Tools[1].ServerToolName() != "web_fetch" {
		t.Fatalf("families = %q/%q", req.Tools[0].ServerToolName(), req.Tools[1].ServerToolName())
	}
	out, err := ResponsesCodec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got struct {
		Tools []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("encoded body: %v", err)
	}
	if len(got.Tools) != 2 || got.Tools[0].Type != "web_search_preview" || got.Tools[1].Type != "web_fetch" {
		t.Fatalf("native server tools not preserved: %+v", got.Tools)
	}
}

func decodeObject(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("encoded body is not a JSON object: %v (%s)", err, raw)
	}
	return got
}

func assertJSONField(t *testing.T, got map[string]json.RawMessage, name, want string) {
	t.Helper()
	raw, ok := got[name]
	if !ok {
		t.Fatalf("field %q missing from %v", name, keys(got))
	}
	var g, w any
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("field %q is not JSON: %v", name, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("test want %q is not JSON: %v", want, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("field %q = %s, want %s", name, raw, want)
	}
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
