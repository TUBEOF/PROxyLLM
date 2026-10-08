package httpapi

// C1 regression (2026-10-07 review): the emulation detail is
// upstream-influenced text (a Firecrawl error string, a model-supplied URL)
// and the child row is a curated surface of its own — it must be masked
// before it is stored, exactly like ErrorRedacted. A route-level reason
// carries no secret material and must survive unmangled.

import (
	"strings"
	"testing"
)

func TestChildOpsFromTrace_RedactsEmulationDetail(t *testing.T) {
	trace := []byte(`{"version":6,"events":[
		{"type":"emulated_tool","to":"web_search","reason":"emulator search failed: firecrawl: HTTP 401: unauthorized key sk-live-abcd1234efgh"},
		{"type":"emulated_tool","to":"web_fetch","reason":"fetched https://example.com/a?token=xxxxxxxxxxxxxxxx"},
		{"type":"failover","from":"a","to":"b","reason":"chain failover after rate_limit"}]}`)

	ops := ChildOpsFromTrace(trace)
	if len(ops) != 3 {
		t.Fatalf("ops = %d, want 3: %+v", len(ops), ops)
	}
	if strings.Contains(ops[0].Detail, "sk-live-abcd1234efgh") {
		t.Errorf("emulation error detail persisted a credential: %q", ops[0].Detail)
	}
	if !strings.Contains(ops[0].Detail, "[REDACTED") {
		t.Errorf("emulation error detail not masked: %q", ops[0].Detail)
	}
	if strings.Contains(ops[1].Detail, "xxxxxxxxxxxxxxxx") {
		t.Errorf("fetched URL detail persisted a token: %q", ops[1].Detail)
	}
	// A route-level reason carries no secret material and must survive
	// unmangled — masking must not wreck ordinary text.
	if ops[2].Detail != "chain failover after rate_limit" {
		t.Errorf("failover detail = %q, want the reason verbatim", ops[2].Detail)
	}
}
