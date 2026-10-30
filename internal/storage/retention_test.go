package storage

import (
	"context"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/config"
)

func TestPruneRequestLogs_DeletesOldRowsAndChildren(t *testing.T) {
	st, err := Open(config.StorageConfig{Path: t.TempDir() + "/test.db"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	oldID, err := st.InsertRequestLog(ctx, RequestLog{
		Timestamp: time.Now().Add(-48 * time.Hour).Unix(), Status: 200,
	})
	if err != nil {
		t.Fatalf("insert old row: %v", err)
	}
	freshID, err := st.InsertRequestLog(ctx, RequestLog{
		Timestamp: time.Now().Unix(), Status: 200,
	})
	if err != nil {
		t.Fatalf("insert fresh row: %v", err)
	}
	if _, err := st.InsertChildLogs(ctx, oldID, []ChildLog{{Kind: "failover"}}); err != nil {
		t.Fatalf("insert child: %v", err)
	}

	n, err := st.PruneRequestLogs(ctx, time.Now().Add(-24*time.Hour).Unix())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned = %d, want 1", n)
	}

	count := func(query string, arg int64) int {
		t.Helper()
		var v int
		if err := st.db.QueryRowContext(ctx, query, arg).Scan(&v); err != nil {
			t.Fatalf("count: %v", err)
		}
		return v
	}
	if got := count(`SELECT COUNT(*) FROM request_logs WHERE id = ?`, oldID); got != 0 {
		t.Errorf("old request row survived: %d", got)
	}
	if got := count(`SELECT COUNT(*) FROM request_logs WHERE id = ?`, freshID); got != 1 {
		t.Errorf("fresh request row deleted: %d", got)
	}
	if got := count(`SELECT COUNT(*) FROM request_child_logs WHERE parent_id = ?`, oldID); got != 0 {
		t.Errorf("old child row survived: %d", got)
	}
}
