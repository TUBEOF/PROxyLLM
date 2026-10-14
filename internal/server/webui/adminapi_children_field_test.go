package webui

// A failed sub-operation read must not
// look like "this request had no fallbacks" — that is the observability lie
// the feature exists to prevent. The envelope therefore carries an explicit
// unavailable marker, and this pure helper is the single place that decides
// absence-vs-empty-vs-failed.

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/tubeof/proxyllm/internal/storage"
)

func TestLogChildrenField_ThreeStates(t *testing.T) {
	// Failed read: no field, and the caller must flag it unavailable.
	field, failed := logChildrenField(nil, errors.New("store unavailable"), nil, logNames{})
	if field != nil {
		t.Errorf("field = %+v, want nil on a failed read", field)
	}
	if !failed {
		t.Error("failed = false, want true — a read error must be distinguishable from no children")
	}

	// No children anywhere: no field, not a failure.
	field, failed = logChildrenField(map[int64][]storage.ChildLog{}, nil, nil, logNames{})
	if field != nil || failed {
		t.Errorf("empty map → field=%+v failed=%v, want nil/false (plain absence)", field, failed)
	}

	// Children present: projected, not a failure.
	field, failed = logChildrenField(map[int64][]storage.ChildLog{
		7: {{ID: 1, ParentID: 7, Kind: "failover", Detail: "d"}},
	}, nil, nil, logNames{})
	if failed {
		t.Error("failed = true on a successful read")
	}
	got, ok := field["7"]
	if !ok || len(got) != 1 || got[0].Kind != "failover" {
		t.Errorf("field = %+v, want one failover op keyed by \"7\"", field)
	}
}

// A step of a request carries the request's own facts: the surface it arrived
// on and the client key that opened it. They are the request's, so the child row
// inherits them rather than storing a second copy.
func TestLogChildrenField_InheritsTheRequestsProtocolAndKey(t *testing.T) {
	parents := map[int64]logDTO{
		7: {ID: 7, Protocol: "anthropic", ClientKeyID: "k1", KeyName: "laptop", KeyPrefix: "pk_ab12"},
	}
	field, failed := logChildrenField(map[int64][]storage.ChildLog{
		7: {{
			ID: 1, ParentID: 7, Kind: "emulated_tool", ProviderID: "acc-firecrawl",
			ToRef: "web_search", LatencyMS: 412, Usage: json.RawMessage(`{}`),
		}},
	}, nil, parents, logNames{})
	if failed {
		t.Fatal("failed = true on a successful read")
	}
	got := field["7"]
	if len(got) != 1 {
		t.Fatalf("ops = %d, want 1", len(got))
	}
	op := got[0]
	if op.Protocol != "anthropic" || op.ClientKeyID != "k1" || op.KeyPrefix != "pk_ab12" {
		t.Errorf("inherited = %q/%q/%q, want the parent request's surface and key",
			op.Protocol, op.ClientKeyID, op.KeyPrefix)
	}
	if op.LatencyMS != 412 {
		t.Errorf("latency = %d, want the stored 412 ms", op.LatencyMS)
	}
	// The step names where the call went, never the model of the round that
	// asked for it.
	if op.ProviderID != "acc-firecrawl" || op.Model != "" {
		t.Errorf("endpoint = %q/%q, want the serving account and no model", op.ProviderID, op.Model)
	}
}
