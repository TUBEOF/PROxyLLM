package storage

import (
	"context"
	"testing"
)

// Spec: the /logs route — bulk table with pagination and filters (model,
// provider, status, time range) over request_logs; the admin API serves the
// redacted rows. ListRequestLogs (existing) answers "latest N" only, which
// cannot back a filterable, page-countable viewer.

func seedLogs(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	rows := []RequestLog{
		{Timestamp: 100, ClientKeyID: "k1", ProviderID: "codex-1", Model: "gpt-x", Status: 200, LatencyMS: 10},
		{Timestamp: 200, ClientKeyID: "k1", ProviderID: "codex-1", Model: "gpt-x", Status: 200, LatencyMS: 11},
		{Timestamp: 300, ClientKeyID: "k1", ProviderID: "ollama-1", Model: "llama-y", Status: 500, LatencyMS: 12},
		{Timestamp: 400, ClientKeyID: "k2", ProviderID: "ollama-1", Model: "llama-y", Status: 200, LatencyMS: 13},
		{Timestamp: 500, ClientKeyID: "k2", ProviderID: "custom-1", Model: "gpt-x", Status: 429, LatencyMS: 14},
	}
	for _, r := range rows {
		if _, err := s.InsertRequestLog(ctx, r); err != nil {
			t.Fatalf("InsertRequestLog: %v", err)
		}
	}
}

func TestRequestLogsPage_NoFilterPaginatesNewestFirst(t *testing.T) {
	s := openTokenStore(t)
	seedLogs(t, s)

	// Page 1 = newest rows; total = all rows, so the UI can render page
	// links without a second query per page.
	rows, total, err := s.ListRequestLogsPage(context.Background(), RequestLogFilter{}, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
	if len(rows) != 2 || rows[0].Timestamp != 500 || rows[1].Timestamp != 400 {
		t.Errorf("page 1: got %+v, want ts 500,400 newest-first", rows)
	}

	rows, _, err = s.ListRequestLogsPage(context.Background(), RequestLogFilter{}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Timestamp != 300 || rows[1].Timestamp != 200 {
		t.Errorf("page 2: got %+v, want ts 300,200", rows)
	}
}

func TestRequestLogsPage_BeyondEndIsEmpty(t *testing.T) {
	s := openTokenStore(t)
	seedLogs(t, s)

	// An offset past the end is a normal UI state (stale page link after
	// retention cleanup): empty page, correct total, no error.
	rows, total, err := s.ListRequestLogsPage(context.Background(), RequestLogFilter{}, 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 || len(rows) != 0 {
		t.Errorf("got rows=%d total=%d, want 0 rows, total 5", len(rows), total)
	}
}

func TestRequestLogsPage_FilterByModel(t *testing.T) {
	s := openTokenStore(t)
	seedLogs(t, s)

	rows, total, err := s.ListRequestLogsPage(context.Background(), RequestLogFilter{Model: "llama-y"}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("got total=%d rows=%d, want 2/2", total, len(rows))
	}
	for _, r := range rows {
		if r.Model != "llama-y" {
			t.Errorf("leaked row: %+v", r)
		}
	}
}

func TestRequestLogsPage_FilterByProvider(t *testing.T) {
	s := openTokenStore(t)
	seedLogs(t, s)

	rows, total, err := s.ListRequestLogsPage(context.Background(), RequestLogFilter{ProviderID: "ollama-1"}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("got total=%d rows=%d, want 2/2", total, len(rows))
	}
	for _, r := range rows {
		if r.ProviderID != "ollama-1" {
			t.Errorf("leaked row: %+v", r)
		}
	}
}

func TestRequestLogsPage_FilterByStatus(t *testing.T) {
	s := openTokenStore(t)
	seedLogs(t, s)

	// The "failures" view of the logs page: status filter over a
	// pointer-free helper — 0 means "any".
	rows, total, err := s.ListRequestLogsPage(context.Background(), RequestLogFilter{Status: 500}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(rows) != 1 || rows[0].Timestamp != 300 {
		t.Errorf("status filter: got total=%d rows=%+v", total, rows)
	}
}

func TestRequestLogsPage_FilterByTimeRange(t *testing.T) {
	s := openTokenStore(t)
	seedLogs(t, s)

	rows, total, err := s.ListRequestLogsPage(context.Background(), RequestLogFilter{Since: 200, Until: 400}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(rows) != 3 {
		t.Fatalf("got total=%d rows=%d, want 3/3", total, len(rows))
	}
	for _, r := range rows {
		if r.Timestamp < 200 || r.Timestamp > 400 {
			t.Errorf("leaked row: %+v", r)
		}
	}
}

func TestRequestLogsPage_CombinedFilters(t *testing.T) {
	s := openTokenStore(t)
	seedLogs(t, s)

	// Filters AND together — the UI applies them simultaneously.
	rows, total, err := s.ListRequestLogsPage(context.Background(),
		RequestLogFilter{Model: "gpt-x", ProviderID: "codex-1", Status: 200}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("got total=%d rows=%d, want 2/2", total, len(rows))
	}
}
