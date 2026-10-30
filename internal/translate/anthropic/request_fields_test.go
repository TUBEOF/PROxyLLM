package anthropic

// Request-field coverage for the cross-protocol controls: Anthropic top_k,
// metadata and service_tier round-trip, unknown fields pass through
// same-protocol, and a mapped reasoning effort becomes a thinking block.

import (
	"encoding/json"
	"testing"

	"github.com/tubeof/proxyllm/internal/translate/ir"
)

func TestRequestFieldsRoundTrip(t *testing.T) {
	raw := `{
		"model":"claude-sonnet-5",
		"max_tokens":100,
		"messages":[{"role":"user","content":"hi"}],
		"top_k":5,
		"metadata":{"user_id":"u1","other":"x"},
		"service_tier":"standard_only",
		"custom_field":{"a":1}
	}`
	req, err := Codec{}.DecodeRequest([]byte(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.TopK == nil || *req.TopK != 5 {
		t.Errorf("TopK = %v, want 5", req.TopK)
	}
	if req.Metadata["user_id"] != "u1" {
		t.Errorf("Metadata = %v", req.Metadata)
	}
	out, err := Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("encoded body not an object: %v", err)
	}
	for name, want := range map[string]string{
		"top_k":        `5`,
		"metadata":     `{"user_id":"u1"}`,
		"service_tier": `"standard_only"`,
		"custom_field": `{"a":1}`,
	} {
		rawField, ok := got[name]
		if !ok {
			t.Errorf("field %q missing after round trip", name)
			continue
		}
		var g, w any
		_ = json.Unmarshal(rawField, &g)
		_ = json.Unmarshal([]byte(want), &w)
		if string(rawField) != want && !jsonEqual(g, w) {
			t.Errorf("field %q = %s, want %s", name, rawField, want)
		}
	}
}

func TestReasoningEffortBecomesThinking(t *testing.T) {
	req := ir.Request{Model: "claude-sonnet-5", MaxTokens: 10000, ReasoningEffort: "high"}
	out, err := Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got struct {
		Thinking *struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode encoded body: %v", err)
	}
	if got.Thinking == nil || got.Thinking.Type != "enabled" || got.Thinking.BudgetTokens != 8192 {
		t.Fatalf("thinking = %+v, want enabled/8192", got.Thinking)
	}
}

func TestCrossProtocolSanitizesMetadataAndTier(t *testing.T) {
	req := ir.Request{
		Model:       "claude-sonnet-5",
		MaxTokens:   100,
		Metadata:    map[string]string{"user_id": "u", "other": "x"},
		ServiceTier: "flex",
	}
	out, err := Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got struct {
		Metadata    map[string]string `json:"metadata"`
		ServiceTier string            `json:"service_tier"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode encoded body: %v", err)
	}
	if len(got.Metadata) != 1 || got.Metadata["user_id"] != "u" {
		t.Errorf("metadata = %v, want only user_id", got.Metadata)
	}
	if got.ServiceTier != "" {
		t.Errorf("service_tier = %q, want dropped (Anthropic enum)", got.ServiceTier)
	}
}

func TestMaxTokensDefaultAndBudgetClamp(t *testing.T) {
	out, err := Codec{}.EncodeRequest(ir.Request{Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("encode default: %v", err)
	}
	var got struct {
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode default: %v", err)
	}
	if got.MaxTokens != defaultMaxTokens {
		t.Errorf("max_tokens = %d, want %d", got.MaxTokens, defaultMaxTokens)
	}

	out, err = Codec{}.EncodeRequest(ir.Request{
		Model: "claude-sonnet-5", MaxTokens: 100, ReasoningEffort: "high",
	})
	if err != nil {
		t.Fatalf("encode clamp: %v", err)
	}
	var clamped struct {
		Thinking struct {
			BudgetTokens int `json:"budget_tokens"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(out, &clamped); err != nil {
		t.Fatalf("decode clamp: %v", err)
	}
	if clamped.Thinking.BudgetTokens != 99 {
		t.Errorf("budget = %d, want max_tokens-1 = 99", clamped.Thinking.BudgetTokens)
	}
}

func jsonEqual(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}
