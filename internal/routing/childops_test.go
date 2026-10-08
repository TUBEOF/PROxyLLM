package routing

// Spec source: the route_trace event vocabulary (trace.go:24-93) and the
// operator requirement of 2026-10-07 — a fallback model walk (vision), an
// emulated web-tool call, and a chain failover must each become their own
// log row instead of living only inside the parent's trace. The mapping
// below is the single source of truth shared by the persist path
// (httpapi.writeRequestLog) and the live feed (server/livefeed), so the two
// can never disagree about what a sub-operation is.
//
// Test doctrine (ADR-006): these cases are derived from the documented
// meaning of each event type, not from the current implementation's output.

import "testing"

// TestChildOps_CleanRequestHasNoOps pins that a request which simply
// succeeded through its primary provider produces no sub-operations — the
// ordinary case must not clutter the log with synthetic rows.
func TestChildOps_CleanRequestHasNoOps(t *testing.T) {
	events := []Event{
		{Type: EventAlias, From: "alias1", To: "glm-5.3"},
		{Type: EventModelResolved, From: "ollamacloud/glm-5.3", To: "glm-5.3"},
		{Type: EventAttempt, ProviderID: "acc-a", Model: "glm-5.3"},
	}

	ops := ChildOps(events)

	if len(ops) != 0 {
		t.Fatalf("ChildOps(clean request) = %d ops, want 0: %+v", len(ops), ops)
	}
}

// TestChildOps_Failover pins the chain failover op: the transition from the
// failed entry to the next one becomes its own row, carrying both provider
// ids and the executor's reason ("chain failover after rate_limit").
func TestChildOps_Failover(t *testing.T) {
	events := []Event{
		{Type: EventAttempt, ProviderID: "acc-a", Model: "glm-5.3"},
		{Type: EventFailed, ProviderID: "acc-a", HTTPStatus: 429, ErrorCode: "rate_limit"},
		{Type: EventFailover, From: "acc-a", To: "acc-b", Reason: "chain failover after rate_limit"},
		{Type: EventAttempt, ProviderID: "acc-b", Model: "glm-5.3"},
	}

	ops := ChildOps(events)

	if len(ops) != 1 {
		t.Fatalf("ChildOps = %d ops, want 1: %+v", len(ops), ops)
	}
	got := ops[0]
	if got.Kind != OpFailover {
		t.Errorf("Kind = %q, want %q", got.Kind, OpFailover)
	}
	if got.From != "acc-a" || got.To != "acc-b" {
		t.Errorf("From/To = %q/%q, want acc-a/acc-b", got.From, got.To)
	}
	if got.Detail != "chain failover after rate_limit" {
		t.Errorf("Detail = %q, want the executor's failover reason", got.Detail)
	}
}

// TestChildOps_CapabilityFallback pins the vision-fallback consultation: the
// capability family is the op's specific fact (the event's reason is the
// same boilerplate for every consultation), and no provider is named yet —
// the providers arrive as the follow-on failover ops.
func TestChildOps_CapabilityFallback(t *testing.T) {
	events := []Event{
		{Type: EventCapabilityFallback, Capability: "vision",
			Reason: "chain exhausted on capability gaps; trying the capability fallback models"},
		{Type: EventFailover, From: "acc-a", To: "vision-acc", Reason: "capability fallback model"},
	}

	ops := ChildOps(events)

	if len(ops) != 2 {
		t.Fatalf("ChildOps = %d ops, want 2 (consultation + fallback model): %+v", len(ops), ops)
	}
	if ops[0].Kind != OpCapabilityFallback {
		t.Errorf("ops[0].Kind = %q, want %q", ops[0].Kind, OpCapabilityFallback)
	}
	if ops[0].Detail != "vision" {
		t.Errorf("ops[0].Detail = %q, want the capability family %q", ops[0].Detail, "vision")
	}
	if ops[0].To != "" {
		t.Errorf("ops[0].To = %q, want empty (no target provider is named by the consultation)", ops[0].To)
	}
	if ops[1].Kind != OpFailover || ops[1].To != "vision-acc" {
		t.Errorf("ops[1] = %+v, want a failover onto the fallback model", ops[1])
	}
}

// TestChildOps_EmulatedTool pins the emulated web-tool call: the tool name is
// the target and the emulation detail (query/url/outcome) is the detail. Both
// a success and an error carry the same shape.
func TestChildOps_EmulatedTool(t *testing.T) {
	events := []Event{
		{Type: EventEmulatedTool, To: "web_search", Reason: `query "golang" → 5 result(s)`},
		{Type: EventEmulatedTool, To: "web_fetch", Reason: "error url_not_allowed: host not permitted"},
	}

	ops := ChildOps(events)

	if len(ops) != 2 {
		t.Fatalf("ChildOps = %d ops, want 2: %+v", len(ops), ops)
	}
	for i, want := range []struct{ tool, detail string }{
		{"web_search", `query "golang" → 5 result(s)`},
		{"web_fetch", "error url_not_allowed: host not permitted"},
	} {
		if ops[i].Kind != OpEmulatedTool {
			t.Errorf("ops[%d].Kind = %q, want %q", i, ops[i].Kind, OpEmulatedTool)
		}
		if ops[i].To != want.tool {
			t.Errorf("ops[%d].To = %q, want %q", i, ops[i].To, want.tool)
		}
		if ops[i].Detail != want.detail {
			t.Errorf("ops[%d].Detail = %q, want %q", i, ops[i].Detail, want.detail)
		}
	}
}

// TestChildOps_Retry pins the same-provider backoff retry as its own op.
func TestChildOps_Retry(t *testing.T) {
	events := []Event{
		{Type: EventRetry, ProviderID: "acc-a", Reason: "retry after 503 (backoff)"},
	}

	ops := ChildOps(events)

	if len(ops) != 1 {
		t.Fatalf("ChildOps = %d ops, want 1: %+v", len(ops), ops)
	}
	if ops[0].Kind != OpRetry || ops[0].ProviderID != "acc-a" {
		t.Errorf("op = %+v, want a retry op for acc-a", ops[0])
	}
	if ops[0].Detail != "retry after 503 (backoff)" {
		t.Errorf("Detail = %q, want the retry reason", ops[0].Detail)
	}
}

// TestChildOps_PreservesOrder pins that ops come out in trace order — the
// frontend renders them as a sequence, so the order is part of the contract.
func TestChildOps_PreservesOrder(t *testing.T) {
	events := []Event{
		{Type: EventFailover, From: "a1", To: "a2", Reason: "first"},
		{Type: EventEmulatedTool, To: "web_search", Reason: "second"},
		{Type: EventRetry, ProviderID: "a2", Reason: "third"},
	}

	ops := ChildOps(events)

	if len(ops) != 3 {
		t.Fatalf("ChildOps = %d ops, want 3: %+v", len(ops), ops)
	}
	for i, want := range []string{"first", "second", "third"} {
		if ops[i].Detail != want {
			t.Errorf("ops[%d].Detail = %q, want %q (trace order lost)", i, ops[i].Detail, want)
		}
	}
}

// TestChildOps_IgnoresAttemptBookkeeping pins that the ops slice never
// contains an op for a kind the operator did not ask for: attempt/failed/
// alias/model_resolved/skipped/expand/rotation/compat_policy/rejected stay
// timeline events in the detail drawer, not rows in the log.
func TestChildOps_IgnoresAttemptBookkeeping(t *testing.T) {
	events := []Event{
		{Type: EventAttempt, ProviderID: "acc-a"},
		{Type: EventFailed, ProviderID: "acc-a", HTTPStatus: 500},
		{Type: EventAlias, From: "x", To: "y"},
		{Type: EventModelResolved, From: "x", To: "y"},
		{Type: EventSkipped, ProviderID: "acc-c", Reason: "cooldown"},
		{Type: EventExpand, From: "t", To: "a,b"},
		{Type: EventRotation, From: "a", To: "b"},
		{Type: EventCompatPolicy, From: "thinking", To: "strip"},
		{Type: EventRejected, Reason: "no route"},
		{Type: EventContextOverride, From: "m[1m]", To: "m"},
		{Type: EventImplicitRoute, From: "m", To: "acc-a"},
	}

	ops := ChildOps(events)

	if len(ops) != 0 {
		t.Fatalf("ChildOps(bookkeeping only) = %d ops, want 0: %+v", len(ops), ops)
	}
}
