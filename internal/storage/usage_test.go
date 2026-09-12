package storage

import (
	"context"
	"testing"
)

// Spec: plan Analysis C schema — usage_daily(day, client_key_id, provider_id,
// model, input_tokens, output_tokens, cache_read_tokens, requests, failures)
// is the Overview dashboard's data source (§9: KPI cards + charts need honest
// aggregates, never fabricated metrics). The proxy's request-log path
// increments rows per completion; the WebUI queries day ranges.

func TestUsageDaily_IncrementAccumulates(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	// Two completions for the same (day, key, provider, model) tuple must
	// land in ONE row with summed counters — the PK is the tuple, so the
	// upsert must accumulate, not overwrite.
	first := UsageDaily{
		Day: "2026-09-12", ClientKeyID: "key-1", ProviderID: "codex-1", Model: "gpt-x",
		InputTokens: 100, OutputTokens: 50, CacheReadTokens: 10, Requests: 1, Failures: 0,
	}
	if err := s.IncrUsageDaily(ctx, first); err != nil {
		t.Fatalf("IncrUsageDaily: %v", err)
	}
	second := first
	second.InputTokens, second.OutputTokens, second.Requests, second.Failures = 200, 25, 1, 1
	if err := s.IncrUsageDaily(ctx, second); err != nil {
		t.Fatal(err)
	}

	rows, err := s.QueryUsageDaily(ctx, "2026-09-01", "2026-09-30")
	if err != nil {
		t.Fatalf("QueryUsageDaily: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (same tuple accumulates)", len(rows))
	}
	r := rows[0]
	if r.InputTokens != 300 || r.OutputTokens != 75 || r.CacheReadTokens != 20 {
		t.Errorf("tokens: got in=%d out=%d cache=%d, want 300/75/20 (both increments carry cache=10)", r.InputTokens, r.OutputTokens, r.CacheReadTokens)
	}
	if r.Requests != 2 || r.Failures != 1 {
		t.Errorf("requests/failures: got %d/%d, want 2/1", r.Requests, r.Failures)
	}
}

func TestUsageDaily_DistinctTuplesAreDistinctRows(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	base := UsageDaily{Day: "2026-09-12", ClientKeyID: "key-1", ProviderID: "codex-1", Model: "gpt-x", Requests: 1}
	if err := s.IncrUsageDaily(ctx, base); err != nil {
		t.Fatal(err)
	}
	otherModel := base
	otherModel.Model = "gpt-y"
	if err := s.IncrUsageDaily(ctx, otherModel); err != nil {
		t.Fatal(err)
	}
	otherKey := base
	otherKey.ClientKeyID = "key-2"
	if err := s.IncrUsageDaily(ctx, otherKey); err != nil {
		t.Fatal(err)
	}

	rows, err := s.QueryUsageDaily(ctx, "2026-09-12", "2026-09-12")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Errorf("rows = %d, want 3 (PK is the full tuple)", len(rows))
	}
}

func TestUsageDaily_QueryRangeIsInclusiveAndOrdered(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	for _, day := range []string{"2026-09-10", "2026-09-11", "2026-09-12", "2026-09-13"} {
		if err := s.IncrUsageDaily(ctx, UsageDaily{Day: day, ClientKeyID: "key-1", ProviderID: "codex-1", Model: "m", Requests: 1}); err != nil {
			t.Fatal(err)
		}
	}

	// Inclusive bounds on both ends; ascending day order so the dashboard
	// chart renders a stable time axis.
	rows, err := s.QueryUsageDaily(ctx, "2026-09-11", "2026-09-13")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	for i, want := range []string{"2026-09-11", "2026-09-12", "2026-09-13"} {
		if rows[i].Day != want {
			t.Errorf("rows[%d].Day = %q, want %q", i, rows[i].Day, want)
		}
	}
}

func TestUsageDaily_QueryOutsideRangeEmpty(t *testing.T) {
	s := openTokenStore(t)

	if err := s.IncrUsageDaily(context.Background(), UsageDaily{Day: "2026-09-12", ClientKeyID: "k", ProviderID: "codex-1", Model: "m", Requests: 1}); err != nil {
		t.Fatal(err)
	}
	// A fresh deployment's dashboard shows an honest empty state, not an
	// error: empty slice, never nil-map panic, for a matching-free range.
	rows, err := s.QueryUsageDaily(context.Background(), "2025-01-01", "2025-01-31")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %d, want 0", len(rows))
	}
}
