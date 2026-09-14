package httpapi

// Task 5 Gap #3 (plan Analysis C schema): the request-completion path
// increments usage_daily — the Overview dashboard's data source. Tests
// are spec-derived: the schema contract is the (day, client_key_id,
// provider_id, model) tuple with the five counters; the writer must fire
// on success AND failure rows (Analysis E: every observable request
// counts), never break the already-sent response.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/proxyllm/proxyllm/internal/storage"
)

// usageRows reads the usage_daily table.
func usageRows(t *testing.T, e *env) []storage.UsageDaily {
	t.Helper()
	today := time.Now().UTC().Format("2006-01-02")
	rows, err := e.store.QueryUsageDaily(t.Context(), today, today)
	if err != nil {
		t.Fatalf("query usage: %v", err)
	}
	return rows
}

// Success path: a non-streaming chat request increments requests (+1),
// input/output tokens, and cache-read tokens from the upstream usage
// (O4 usage semantics, translated by the codec into IR usage).
func TestUsageDaily_IncrementedOnChatSuccess(t *testing.T) {
	e := newEnv(t)
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))
	status, body := e.post(t, "/v1/chat/completions", string(golden(t, "openai/chat-request.json")))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", status, body)
	}

	rows := usageRows(t, e)
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1: %+v", len(rows), rows)
	}
	u := rows[0]
	if u.Day != time.Now().UTC().Format("2006-01-02") {
		t.Errorf("day = %q, want today (UTC)", u.Day)
	}
	if u.Model != "gpt-5" {
		t.Errorf("model = %q, want gpt-5", u.Model)
	}
	if u.Requests != 1 || u.Failures != 0 {
		t.Errorf("requests/failures = %d/%d, want 1/0", u.Requests, u.Failures)
	}
	if u.InputTokens != 1250 || u.OutputTokens != 77 {
		t.Errorf("tokens = %d/%d, want 1250/77 (golden usage)", u.InputTokens, u.OutputTokens)
	}
	if u.CacheReadTokens != 8192 {
		t.Errorf("cache_read = %d, want 8192 (golden prompt_tokens_details.cached_tokens)", u.CacheReadTokens)
	}
	if u.ProviderID != "mock-main" {
		t.Errorf("provider = %q, want mock-main", u.ProviderID)
	}
}

// Failure path: a failed request is still an observable request (Analysis
// E) — requests+1 AND failures+1 on the same tuple.
func TestUsageDaily_IncrementedOnFailure(t *testing.T) {
	e := newEnv(t)
	e.mock.Enqueue(mockFail(t, 500, string(golden(t, "openai/error.json"))))
	e.post(t, "/v1/chat/completions", string(golden(t, "openai/chat-request.json")))

	rows := usageRows(t, e)
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1: %+v", len(rows), rows)
	}
	if rows[0].Requests != 1 || rows[0].Failures != 1 {
		t.Errorf("requests/failures = %d/%d, want 1/1", rows[0].Requests, rows[0].Failures)
	}
}

// Streaming path: the terminal usage from the SSE frame increments the
// same counters (O4 stream_options.include_usage).
func TestUsageDaily_IncrementedOnStreamSuccess(t *testing.T) {
	e := newEnv(t)
	e.mock.Enqueue(mockSSE(t, string(golden(t, "openai/chat-stream.sse"))))
	status, body := e.postStream(t, "/v1/chat/completions", streamReq(t, string(golden(t, "openai/chat-request.json"))))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", status, body)
	}

	rows := usageRows(t, e)
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1: %+v", len(rows), rows)
	}
	if rows[0].Requests != 1 || rows[0].Failures != 0 {
		t.Errorf("requests/failures = %d/%d, want 1/0", rows[0].Requests, rows[0].Failures)
	}
	if rows[0].InputTokens != 1250 || rows[0].OutputTokens != 128 {
		t.Errorf("tokens = %d/%d, want 1250/128 (golden stream usage)", rows[0].InputTokens, rows[0].OutputTokens)
	}
}

// Anthropic surface: /v1/messages increments the same aggregation (both
// surfaces feed one dashboard).
func TestUsageDaily_IncrementedOnMessagesSuccess(t *testing.T) {
	e := newClaudeEnv(t)
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "anthropic/message-response.json"))))
	status, body := e.post(t, "/v1/messages", string(golden(t, "anthropic/message-request.json")))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", status, body)
	}

	rows := usageRows(t, e)
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1: %+v", len(rows), rows)
	}
	if rows[0].Model != "claude-sonnet-5" {
		t.Errorf("model = %q, want claude-sonnet-5", rows[0].Model)
	}
	if rows[0].Requests != 1 || rows[0].Failures != 0 {
		t.Errorf("requests/failures = %d/%d, want 1/0", rows[0].Requests, rows[0].Failures)
	}
}

// Two requests with the same tuple accumulate into ONE row (the PK is the
// full tuple — no row-per-request drift).
func TestUsageDaily_AccumulatesSameTuple(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 2; i++ {
		e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))
		if status, body := e.post(t, "/v1/chat/completions", string(golden(t, "openai/chat-request.json"))); status != http.StatusOK {
			t.Fatalf("request %d: status %d (%s)", i, status, body)
		}
	}

	rows := usageRows(t, e)
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1 (tuple PK accumulates): %+v", len(rows), rows)
	}
	if rows[0].Requests != 2 || rows[0].InputTokens != 2500 {
		t.Errorf("accumulated requests/tokens = %d/%d, want 2/2500", rows[0].Requests, rows[0].InputTokens)
	}
}

// streamReq adds stream:true to a chat request body.
func streamReq(t *testing.T, body string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	m["stream"] = true
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
