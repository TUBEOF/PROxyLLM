package webui

// Overview snapshot endpoint (2026-10-06 live updates): one assembled
// request for the Dashboard poll — usage of the trailing 14 UTC days and
// today's totals, so the Overview page refreshes with a single GET and
// the fixturesrv can seed it deterministically (wall-clock-free math on
// the injected Now).

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/storage"
)

func TestOverview_Snapshot(t *testing.T) {
	e := newAdminEnv(t)
	// Fixed clock: Oct 6 2026; the window is the trailing 14 days from
	// the clock (injected Now — no wall clock anywhere here).
	e.now = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC).Unix()
	s := e.bootstrap(t, "admin")

	ctx := context.Background()
	// One seeded account with usage on two days (in-window) and one
	// outside the window.
	if err := e.st.EnsureProvider(ctx, "acct1", "ollama-cloud", "Acc 1"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	for _, u := range []storage.UsageDaily{
		{Day: "2026-10-06", ProviderID: "acct1", Model: "glm", Requests: 10, InputTokens: 100, OutputTokens: 200},
		{Day: "2026-10-05", ProviderID: "acct1", Model: "glm", Requests: 5, InputTokens: 50, OutputTokens: 70},
		{Day: "2026-09-20", ProviderID: "acct1", Model: "glm", Requests: 99}, // outside the 14-day window
	} {
		if err := e.st.IncrUsageDaily(ctx, u); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	w := e.authedCall(t, http.MethodGet, "/api/admin/overview", "", s)
	assertStatus(t, w, http.StatusOK, "overview")
	var got struct {
		GeneratedAt int64 `json:"generated_at"`
		// Day-ascending rows of the window (day-keyed aggregate of the
		// raw usage_daily rows).
		Usage []usageDTO `json:"usage"`
		Today struct {
			Requests  int64 `json:"requests"`
			Failures  int64 `json:"failures"`
			InputTok  int64 `json:"input_tokens"`
			OutputTok int64 `json:"output_tokens"`
		} `json:"today"`
	}
	decodeJSON(t, w, &got)
	if got.GeneratedAt != e.now {
		t.Fatalf("generated_at = %d, want the injected clock", got.GeneratedAt)
	}
	// Rows: the two in-window days, outside rows excluded.
	if len(got.Usage) != 2 {
		t.Fatalf("usage rows = %d, want 2", len(got.Usage))
	}
	var dayReq int64
	for _, row := range got.Usage {
		if row.Day == "2026-10-06" {
			dayReq += row.Requests
		}
	}
	if dayReq != 10 {
		t.Fatalf("today requests = %d, want 10", dayReq)
	}
	if got.Today.Requests != 10 || got.Today.InputTok != 100 || got.Today.OutputTok != 200 {
		t.Fatalf("today = %+v", got.Today)
	}
}

// TestLogs_AfterCursorFeed: the live-update cursor — after=<rowid>
// returns only NEWER rows (newest-first), total omitted; a bad cursor is
// a 400.
func TestLogs_AfterCursorFeed(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")
	for i := 0; i < 3; i++ {
		if _, err := e.st.InsertRequestLog(context.Background(), testLogRow(i)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	var res struct {
		Logs []logDTO `json:"logs"`
	}
	w := e.authedCall(t, http.MethodGet, "/api/admin/logs?after=1", "", s)
	assertStatus(t, w, http.StatusOK, "after cursor")
	decodeJSON(t, w, &res)
	if len(res.Logs) != 2 || res.Logs[0].ID != 3 || res.Logs[1].ID != 2 {
		t.Fatalf("after=1 logs = %+v, want ids 3,2", res.Logs)
	}

	w = e.authedCall(t, http.MethodGet, "/api/admin/logs?after=abc", "", s)
	assertStatus(t, w, http.StatusBadRequest, "bad cursor")
}

// testLogRow builds one insertable log row (id auto-increments in order).
func testLogRow(i int) storage.RequestLog {
	return storage.RequestLog{
		Timestamp: int64(1000 + i), Status: 200, Model: "m", ProviderID: "p1",
	}
}
