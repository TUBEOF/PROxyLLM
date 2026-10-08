package webui

// I3 regression (2026-10-07 review): a failed sub-operation read must not
// look like "this request had no fallbacks" — that is the observability lie
// the feature exists to prevent. The envelope therefore carries an explicit
// unavailable marker, and this pure helper is the single place that decides
// absence-vs-empty-vs-failed.

import (
	"errors"
	"testing"

	"github.com/tubeof/proxyllm/internal/storage"
)

func TestLogChildrenField_ThreeStates(t *testing.T) {
	// Failed read: no field, and the caller must flag it unavailable.
	field, failed := logChildrenField(nil, errors.New("store unavailable"))
	if field != nil {
		t.Errorf("field = %+v, want nil on a failed read", field)
	}
	if !failed {
		t.Error("failed = false, want true — a read error must be distinguishable from no children")
	}

	// No children anywhere: no field, not a failure.
	field, failed = logChildrenField(map[int64][]storage.ChildLog{}, nil)
	if field != nil || failed {
		t.Errorf("empty map → field=%+v failed=%v, want nil/false (plain absence)", field, failed)
	}

	// Children present: projected, not a failure.
	field, failed = logChildrenField(map[int64][]storage.ChildLog{
		7: {{ID: 1, ParentID: 7, Kind: "failover", Detail: "d"}},
	}, nil)
	if failed {
		t.Error("failed = true on a successful read")
	}
	got, ok := field["7"]
	if !ok || len(got) != 1 || got[0].Kind != "failover" {
		t.Errorf("field = %+v, want one failover op keyed by \"7\"", field)
	}
}
