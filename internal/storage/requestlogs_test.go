package storage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

// Spec: plan Analysis C schema (request_logs: ts, client_key_id, provider_id,
// model, status, latency_ms, usage JSON, route_trace JSON, error_redacted)
// + Analysis E / FO-4 (route_trace must persist the observable routing
// decisions — fallbacks are never silent) + Analysis G (only the redacted
// error text ever reaches this table).

func TestRequestLog_InsertAndFetch(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	in := RequestLog{
		Timestamp:     1760000000,
		ClientKeyID:   "key-1",
		ProviderID:    "codex-1",
		Model:         "gpt-5",
		Status:        200,
		LatencyMS:     412,
		Usage:         json.RawMessage(`{"input_tokens":10,"output_tokens":20}`),
		RouteTrace:    json.RawMessage(`[{"type":"attempt","provider_id":"codex-1"}]`),
		ErrorRedacted: "",
	}
	id, err := s.InsertRequestLog(ctx, in)
	if err != nil {
		t.Fatalf("InsertRequestLog: %v", err)
	}
	if id <= 0 {
		t.Errorf("log id = %d, want a positive autoincrement id", id)
	}

	logs, err := s.ListRequestLogs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("ListRequestLogs = %d rows, want 1", len(logs))
	}
	got := logs[0]
	if got.ID != id || got.Timestamp != in.Timestamp || got.ClientKeyID != in.ClientKeyID ||
		got.ProviderID != in.ProviderID || got.Model != in.Model || got.Status != in.Status ||
		got.LatencyMS != in.LatencyMS {
		t.Errorf("round-trip mismatch: got %+v want %+v", got, in)
	}
	if string(got.Usage) != string(in.Usage) {
		t.Errorf("usage round-trip: %s", got.Usage)
	}
	if string(got.RouteTrace) != string(in.RouteTrace) {
		t.Errorf("route_trace round-trip: %s", got.RouteTrace)
	}
}

func TestRequestLog_ListOrdersNewestFirst(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := s.InsertRequestLog(ctx, RequestLog{Timestamp: int64(1700000000 + i), Status: 200}); err != nil {
			t.Fatal(err)
		}
	}

	logs, err := s.ListRequestLogs(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	// WebUI log viewer: newest first (ts DESC) — id DESC as tie-break keeps
	// insertion order stable for equal timestamps.
	for i := 1; i < len(logs); i++ {
		if logs[i-1].Timestamp < logs[i].Timestamp {
			t.Errorf("logs not newest-first: [%d] ts=%d after [%d] ts=%d", i-1, logs[i-1].Timestamp, i, logs[i].Timestamp)
		}
	}
}

func TestRequestLog_ListLimit(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	for i := 0; i < 7; i++ {
		if _, err := s.InsertRequestLog(ctx, RequestLog{Timestamp: int64(1700000000 + i), Status: 200}); err != nil {
			t.Fatal(err)
		}
	}

	logs, err := s.ListRequestLogs(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 3 {
		t.Fatalf("limit 3 returned %d rows, want 3", len(logs))
	}
	// The limit must keep the NEWEST rows, not the oldest.
	if logs[0].Timestamp != int64(1700000006) {
		t.Errorf("limit kept oldest rows: first ts = %d", logs[0].Timestamp)
	}
}

// Spec: Logs-page redesign (2026-10-06 plan) — request_rows carry the
// per-request protocol surface, feature flags, and conversation identity;
// conversation ids derive from conv_key via a 30-minute rolling window
// (same key + conv_key within the window reuses the prior conversation id),
// and the migration must be append-only (re-open is a no-op).

func TestRequestLog_NewColumnsRoundTrip(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	in := RequestLog{
		Timestamp:      1760000000,
		ClientKeyID:    "key-1",
		ProviderID:     "prov-1",
		Model:          "glm-5.3-flash",
		Status:         200,
		LatencyMS:      2100,
		Usage:          json.RawMessage(`{"input_tokens":88211,"output_tokens":72}`),
		RouteTrace:     json.RawMessage(`{"version":6,"events":[]}`),
		ConversationID: "conv_a1b2c3d4",
		ConvKey:        "k1",
		Protocol:       "anthropic",
		Features:       json.RawMessage(`{"streaming":true,"vision":false}`),
	}
	id, err := s.InsertRequestLog(ctx, in)
	if err != nil {
		t.Fatalf("InsertRequestLog: %v", err)
	}

	logs, err := s.ListRequestLogs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("got %d rows, want 1", len(logs))
	}
	got := logs[0]
	if got.ConversationID != in.ConversationID || got.ConvKey != in.ConvKey ||
		got.Protocol != in.Protocol {
		t.Errorf("conversation/conv_key/protocol round-trip: got %+v", got)
	}
	if string(got.Features) != string(in.Features) {
		t.Errorf("features round-trip: %s", got.Features)
	}

	after, err := s.ListRequestLogsAfter(ctx, RequestLogFilter{}, 10, id-1)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].ConversationID != in.ConversationID || after[0].Protocol != in.Protocol {
		t.Errorf("cursor lister round-trip: %+v", after)
	}
	page, total, err := s.ListRequestLogsPage(ctx, RequestLogFilter{}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(page) != 1 || page[0].Features == nil || string(page[0].Features) != string(in.Features) {
		t.Errorf("paged lister round-trip: total=%d page=%+v", total, page)
	}
}

func TestRequestLog_FeaturesDefaultsToEmptyObject(t *testing.T) {
	s := openTokenStore(t)

	if _, err := s.InsertRequestLog(context.Background(), RequestLog{Timestamp: 1760000000, Status: 500}); err != nil {
		t.Fatal(err)
	}
	logs, err := s.ListRequestLogs(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || !json.Valid(logs[0].Features) {
		t.Errorf("features must default to valid JSON, got %q", logs[0].Features)
	}
}

func TestStore_MigrateReopenIsNoop(t *testing.T) {
	// A second store opened on the same DB file must re-run migrate as a
	// no-op (append-only invariant) — new columns stay readable.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "reopen.db")
	s1, err := Open(testCfg(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s1.InsertRequestLog(ctx, RequestLog{
		Timestamp: 1760000000, Status: 200, ConversationID: "conv_11111111",
		ConvKey: "ck", Protocol: "openai", Features: json.RawMessage(`{"tools":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(testCfg(dbPath))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()
	logs, err := s2.ListRequestLogs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].ConversationID != "conv_11111111" || logs[0].Protocol != "openai" {
		t.Errorf("reopen round-trip: %+v", logs)
	}
}

func TestStore_RecentConversation(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()
	const base = int64(1760000000)

	// No prior row → "".
	got, err := s.RecentConversation(ctx, "key-1", "ck-1", base, 1800)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("empty store → %q, want \"\"", got)
	}

	// Seed a row with a conversation id; same key+conv_key inside the
	// window reuses it.
	if _, err := s.InsertRequestLog(ctx, RequestLog{
		Timestamp: base, ClientKeyID: "key-1", Status: 200,
		ConversationID: "conv_11111111", ConvKey: "ck-1",
	}); err != nil {
		t.Fatal(err)
	}
	got, err = s.RecentConversation(ctx, "key-1", "ck-1", base+600, 1800)
	if err != nil {
		t.Fatal(err)
	}
	if got != "conv_11111111" {
		t.Errorf("inside window → %q, want conv_11111111", got)
	}

	// Outside the window (ts beyond prior + within) → "".
	got, err = s.RecentConversation(ctx, "key-1", "ck-1", base+1801, 1800)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("outside window → %q, want \"\"", got)
	}

	// Different client key → "" (keys never share conversations).
	got, err = s.RecentConversation(ctx, "key-2", "ck-1", base+600, 1800)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("different key → %q, want \"\"", got)
	}

	// Different conv_key → "".
	got, err = s.RecentConversation(ctx, "key-1", "ck-2", base+600, 1800)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("different conv_key → %q, want \"\"", got)
	}

	// A row with an empty conversation id must never be picked up itself,
	// and must not block the prior id-carrying row: the query filters
	// conversation_id != '' so pre-patch rows are skipped, not terminal.
	if _, err := s.InsertRequestLog(ctx, RequestLog{
		Timestamp: base + 100, ClientKeyID: "key-1", Status: 200, ConvKey: "ck-1",
	}); err != nil {
		t.Fatal(err)
	}
	got, err = s.RecentConversation(ctx, "key-1", "ck-1", base+200, 1800)
	if err != nil {
		t.Fatal(err)
	}
	if got != "conv_11111111" {
		t.Errorf("empty-id row skipped → %q, want conv_11111111", got)
	}

	// The newest matching row wins (two id-carrying rows: reuse the latest).
	if _, err := s.InsertRequestLog(ctx, RequestLog{
		Timestamp: base + 300, ClientKeyID: "key-1", Status: 200,
		ConversationID: "conv_22222222", ConvKey: "ck-1",
	}); err != nil {
		t.Fatal(err)
	}
	got, err = s.RecentConversation(ctx, "key-1", "ck-1", base+400, 1800)
	if err != nil {
		t.Fatal(err)
	}
	if got != "conv_22222222" {
		t.Errorf("newest wins → %q, want conv_22222222", got)
	}
}

func TestRequestLog_JSONFieldsDefaultEmptyObject(t *testing.T) {
	s := openTokenStore(t)

	// A log row without usage/trace must still round-trip the schema
	// defaults ('{}') as valid JSON, not NULL — the WebUI renders them
	// directly.
	id, err := s.InsertRequestLog(context.Background(), RequestLog{Timestamp: 1760000000, Status: 500})
	if err != nil {
		t.Fatal(err)
	}
	logs, err := s.ListRequestLogs(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].ID != id {
		t.Fatalf("unexpected rows: %+v", logs)
	}
	if !json.Valid(logs[0].Usage) || !json.Valid(logs[0].RouteTrace) {
		t.Errorf("usage/route_trace must be valid JSON defaults, got %q / %q", logs[0].Usage, logs[0].RouteTrace)
	}
}
