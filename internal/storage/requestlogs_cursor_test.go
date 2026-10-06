package storage

// Live-updates cursor (2026-10-06): the log page polls for rows AFTER a
// known id (incremental feed) instead of repaging — `after` is the last
// seen row id, the query returns newer rows newest-first.

import (
	"context"
	"path/filepath"
	"testing"
)

func TestListRequestLogsAfter(t *testing.T) {
	ctx := context.Background()
	st, err := Open(testCfg(filepath.Join(t.TempDir(), "test.db")))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()

	for i := 0; i < 5; i++ {
		if _, err := st.InsertRequestLog(ctx, RequestLog{
			Timestamp: int64(1_000 + i), Status: 200, Model: "m",
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	rows, err := st.ListRequestLogsAfter(ctx, RequestLogFilter{}, 20, 1)
	if err != nil {
		t.Fatalf("after=1: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4 (ids 2..5)", len(rows))
	}
	if rows[0].ID != 5 {
		t.Fatalf("rows[0].id = %d, want 5 (newest first)", rows[0].ID)
	}

	rows, err = st.ListRequestLogsAfter(ctx, RequestLogFilter{}, 20, 5)
	if err != nil {
		t.Fatalf("after=5: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %d, want 0 (nothing after the newest)", len(rows))
	}

	// The filter rides along.
	rows, err = st.ListRequestLogsAfter(ctx, RequestLogFilter{Status: 500}, 20, 0)
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %d, want 0 (all seeded 200)", len(rows))
	}
}
