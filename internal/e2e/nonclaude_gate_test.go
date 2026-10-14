package e2e

// Spec-derived tests for the global non-Claude switch (ADR-008):
// anthropic_surface_non_claude = allow | warn | block, default
// allow. warn annotates route_trace (compat_policy event); block rejects
// with a protocol-conform error and dispatches nothing; a missing or
// invalid setting is allow — the safety net must not de-route on store
// noise.

import (
	"context"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// seedNonClaude stores the switch in the settings KV (the SPA key).
func seedNonClaude(t *testing.T, e *env, value string) {
	t.Helper()
	if err := e.store.SetConfigValue(context.Background(),
		"anthropic_surface_non_claude", value); err != nil {
		t.Fatalf("set config: %v", err)
	}
}

// claudeMessage is one plain (no thinking) Anthropic request aimed at the
// mock-main chat upstream.
func claudeMessage(t *testing.T, e *env, model string) error {
	t.Helper()
	_, err := e.anthrop.Messages.New(context.Background(), anthropic.MessageNewParams{
		MaxTokens: 512,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
		Model:     model,
	})
	return err
}

// 1. Default (no setting at all): a non-Claude model ("gpt-5" on the
// Anthropic surface) routes — the safety net is allow by default.
func TestNonClaudeGate_DefaultAllow(t *testing.T) {
	e := newEnv(t)
	e.mock.Enqueue(mockJSON(t, 200, chatResponseBody(t)))

	if err := claudeMessage(t, e, "gpt-5"); err != nil {
		t.Fatalf("default allow: request must route, got %v", err)
	}
	// allow records nothing in the trace (no policy application occurred).
	logs := e.requestLogs(t)
	if len(logs) == 0 {
		t.Fatal("no request_logs row")
	}
	if traceCompatEvent(t, logs[0].RouteTrace, "anthropic_surface_non_claude", "allow") {
		t.Errorf("allow must not emit a policy event: %s", logs[0].RouteTrace)
	}
}

// 2. warn: the request still routes, and route_trace carries the
// compat_policy event {"from":"anthropic_surface_non_claude","to":"warn"}
// (never silent).
func TestNonClaudeGate_WarnTracesAndRoutes(t *testing.T) {
	e := newEnv(t)
	seedNonClaude(t, e, "warn")
	e.mock.Enqueue(mockJSON(t, 200, chatResponseBody(t)))

	if err := claudeMessage(t, e, "gpt-5"); err != nil {
		t.Fatalf("warn must not block: %v", err)
	}
	logs := e.requestLogs(t)
	if len(logs) == 0 {
		t.Fatal("no request_logs row")
	}
	if !traceCompatEvent(t, logs[0].RouteTrace, "anthropic_surface_non_claude", "warn") {
		t.Errorf("route_trace = %s, want the warn policy event", logs[0].RouteTrace)
	}
}

// 3. block: the request is refused with a protocol-conform 4xx and NO
// upstream request is dispatched.
func TestNonClaudeGate_BlockRejects(t *testing.T) {
	e := newEnv(t)
	seedNonClaude(t, e, "block")

	err := claudeMessage(t, e, "gpt-5")
	if err == nil {
		t.Fatal("block policy let a non-Claude request through")
	}
	if st := httpStatusOfAnthropic(t, err); st < 400 || st >= 500 {
		t.Errorf("status = %d, want a 4xx", st)
	}
	if reqs := e.mock.Requests(); len(reqs) != 0 {
		t.Errorf("block dispatched %d upstream request(s)", len(reqs))
	}
}

// 4. Claude targets are exempt: the switch governs only non-Claude model
// names — a claude-* request under block still routes.
func TestNonClaudeGate_BlockSparesClaudeTargets(t *testing.T) {
	e := newEnv(t)
	seedNonClaude(t, e, "block")
	e.mock.Enqueue(mockJSON(t, 200, chatResponseBody(t)))

	if err := claudeMessage(t, e, "claude-sonnet-5"); err != nil {
		t.Fatalf("claude target under block must still route: %v", err)
	}
}

// 5. An invalid setting value never de-routes (treat as allow) — the gate
// must not fail closed on store noise.
func TestNonClaudeGate_InvalidValueTreatedAsAllow(t *testing.T) {
	e := newEnv(t)
	seedNonClaude(t, e, "yolo")
	e.mock.Enqueue(mockJSON(t, 200, chatResponseBody(t)))

	if err := claudeMessage(t, e, "gpt-5"); err != nil {
		t.Fatalf("invalid setting must fall back to allow: %v", err)
	}
}
