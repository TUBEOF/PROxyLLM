package routing_test

import (
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// Per-model compatibility-policy tests, RED-GREEN. Derived from plan §6.4
// (OP-2 resolution, user-confirmed 2026-09-10; market practice M1–M3):
//
//	thinking_handling: strip (default for non-Claude) / map-to-reasoning-effort
//	  / block — strip removes thinking blocks + the thinking param upstream
//	  (never sends fabricated signatures back); map converts budget →
//	  reasoning_effort where the target supports it; block refuses with a
//	  protocol-conform error.
//	server_tool_emulation: native / emulate-firecrawl / off.
//	max_tokens_clamp: int / off — clamp for targets with lower output limits
//	  (C7: 32000 default for unknown model IDs).
//	cache_control: forward / drop — forward on Anthropic→Anthropic only (C4);
//	  drop logs the degradation.
//
// Every compat-policy application is recorded in the Trace as a
// compat_policy event — never silent (Analysis E observability).

func compatWith(setting string) routing.Compat {
	return routing.Compat{ThinkingHandling: setting}
}

func thinkingReq(budget int) ir.Request {
	return ir.Request{
		Model: "m",
		Thinking: &ir.ThinkingConfig{
			Enabled:      true,
			BudgetTokens: budget,
		},
	}
}

// TestCompat_ThinkingStrip: strip removes the thinking param and thinking
// blocks from history; application is traced.
func TestCompat_ThinkingStrip(t *testing.T) {
	req := thinkingReq(2048)
	// A prior assistant turn with a thinking block rides the history —
	// signatures must never be sent to a non-thinking target (A12).
	req.Messages = []ir.Message{{
		Role: "assistant",
		Parts: []ir.Part{
			ir.ThinkingPart{Thinking: "chain of thought", Signature: "sig-abc"},
			ir.TextPart{Text: "answer"},
		},
	}}

	out, trace, err := routing.ApplyCompat(compatWith(routing.ThinkingStrip), req)
	if err != nil {
		t.Fatalf("ApplyCompat: %v", err)
	}
	if out.Thinking != nil {
		t.Errorf("thinking param = %+v, want stripped", out.Thinking)
	}
	for i, msg := range out.Messages {
		for _, part := range msg.Parts {
			if _, ok := part.(ir.ThinkingPart); ok {
				t.Errorf("messages[%d] still carries a thinking block; strip must remove them", i)
			}
		}
	}
	if !compatTraced(trace, "thinking_handling", "strip") {
		t.Errorf("trace = %+v, want a compat_policy event naming thinking_handling=strip", trace.Events())
	}
}

// TestCompat_ThinkingMap: map-to-reasoning-effort converts the thinking
// budget to a reasoning-effort hint the target understands (L5/L9).
func TestCompat_ThinkingMap(t *testing.T) {
	out, trace, err := routing.ApplyCompat(compatWith(routing.ThinkingMapToEffort), thinkingReq(20000))
	if err != nil {
		t.Fatalf("ApplyCompat: %v", err)
	}
	if out.Thinking != nil {
		t.Errorf("thinking param = %+v, want mapped away", out.Thinking)
	}
	if out.ReasoningEffort == "" {
		t.Error("ReasoningEffort must be set when mapping thinking → effort")
	}
	// A large budget maps to high effort, a small one to low.
	small, _, err := routing.ApplyCompat(compatWith(routing.ThinkingMapToEffort), thinkingReq(2048))
	if err != nil {
		t.Fatalf("ApplyCompat: %v", err)
	}
	if small.ReasoningEffort != "low" || out.ReasoningEffort != "high" {
		t.Errorf("effort mapping: small=%q large=%q, want low/high", small.ReasoningEffort, out.ReasoningEffort)
	}
	if !compatTraced(trace, "thinking_handling", "map-to-reasoning-effort") {
		t.Errorf("trace = %+v, want a compat_policy event naming the mapping", trace.Events())
	}
}

// TestCompat_ThinkingBlock: block refuses the request outright with a
// protocol-conform error (the operator opted into strictness).
func TestCompat_ThinkingBlock(t *testing.T) {
	_, _, err := routing.ApplyCompat(compatWith(routing.ThinkingBlock), thinkingReq(2048))
	if err == nil {
		t.Fatal("block must refuse thinking requests")
	}
	if !strings.Contains(err.Error(), "thinking") {
		t.Errorf("error = %q, want it to name thinking", err)
	}
}

// TestCompat_ThinkingDefaultStrip: an unset policy defaults to strip for
// non-Claude targets (§6.4) — never pass thinking through silently.
func TestCompat_ThinkingDefaultStrip(t *testing.T) {
	out, trace, err := routing.ApplyCompat(routing.Compat{}, thinkingReq(2048))
	if err != nil {
		t.Fatalf("ApplyCompat: %v", err)
	}
	if out.Thinking != nil {
		t.Errorf("default policy must strip thinking, got %+v", out.Thinking)
	}
	if !compatTraced(trace, "thinking_handling", "strip") {
		t.Errorf("trace = %+v, want the default strip traced", trace.Events())
	}
}

// TestCompat_NoThinkingNoOp: requests without thinking pass through every
// policy unchanged and untraced (no degradation to record).
func TestCompat_NoThinkingNoOp(t *testing.T) {
	for _, setting := range []string{routing.ThinkingStrip, routing.ThinkingMapToEffort, routing.ThinkingBlock} {
		out, trace, err := routing.ApplyCompat(compatWith(setting), ir.Request{Model: "m"})
		if err != nil {
			t.Fatalf("ApplyCompat(%s): %v", setting, err)
		}
		if out.Thinking != nil || out.ReasoningEffort != "" {
			t.Errorf("ApplyCompat(%s) touched a thinking-free request: %+v", setting, out)
		}
		if compatTraced(trace, "thinking_handling", "") && setting != routing.ThinkingStrip {
			t.Errorf("ApplyCompat(%s) traced a no-op", setting)
		}
	}
}

// TestCompat_MaxTokensClamp: the clamp caps MaxTokens for targets with
// lower output limits; no clamp → unchanged; application traced.
func TestCompat_MaxTokensClamp(t *testing.T) {
	cases := []struct {
		name  string
		clamp int
		in    int
		want  int
	}{
		{name: "clamp caps", clamp: 8192, in: 32000, want: 8192},
		{name: "below clamp untouched", clamp: 8192, in: 1000, want: 1000},
		{name: "zero = off", clamp: 0, in: 32000, want: 32000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, trace, err := routing.ApplyCompat(routing.Compat{MaxTokensClamp: tc.clamp}, ir.Request{Model: "m", MaxTokens: tc.in})
			if err != nil {
				t.Fatalf("ApplyCompat: %v", err)
			}
			if out.MaxTokens != tc.want {
				t.Errorf("MaxTokens = %d, want %d", out.MaxTokens, tc.want)
			}
			if tc.want != tc.in && !compatTraced(trace, "max_tokens_clamp", "") {
				t.Errorf("trace = %+v, want the clamp traced", trace.Events())
			}
		})
	}
}

// compatTraced reports whether trace carries a compat_policy event for
// setting (value checked when non-empty).
func compatTraced(trace *routing.Trace, setting, value string) bool {
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventCompatPolicy && ev.From == setting &&
			(value == "" || ev.To == value) {
			return true
		}
	}
	return false
}
