package storage

// ADR-012: per-account local usage windows for the Quota page. The page
// needs day-granular usage GROUPED per (day, provider) — the Quota page
// renders session/week/month bars per account from it when no live
// source exists. The PK already carries provider_id; the query is a
// provider-filtered day-range.

import (
	"context"
	"path/filepath"
	"testing"
)

func TestQueryUsageDailyByProvider(t *testing.T) {
	ctx := context.Background()
	st, err := Open(testCfg(filepath.Join(t.TempDir(), "test.db")))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()

	seed := []UsageDaily{
		{Day: "2026-10-01", ProviderID: "acct-a", Model: "m1", Requests: 3, InputTokens: 100, OutputTokens: 50},
		{Day: "2026-10-01", ProviderID: "acct-a", Model: "m2", Requests: 1, InputTokens: 10, OutputTokens: 5},
		{Day: "2026-10-02", ProviderID: "acct-a", Model: "m1", Requests: 2, InputTokens: 40, OutputTokens: 20},
		{Day: "2026-10-02", ProviderID: "acct-b", Model: "m1", Requests: 1, InputTokens: 1, OutputTokens: 1},
		{Day: "2026-09-28", ProviderID: "acct-a", Model: "m1", Requests: 9}, // outside the range
	}
	for _, u := range seed {
		if err := st.IncrUsageDaily(ctx, u); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	rows, err := st.QueryUsageDailyByProvider(ctx, "acct-a", "2026-10-01", "2026-10-02")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (only acct-a in range)", len(rows))
	}
	day1, day2 := int64(0), int64(0)
	for _, r := range rows {
		switch r.Day {
		case "2026-10-01":
			day1 += r.Requests
		case "2026-10-02":
			day2 += r.Requests
		}
	}
	if day1 != 4 || day2 != 2 {
		t.Fatalf("requests per day = %d/%d, want 4/2", day1, day2)
	}
}
