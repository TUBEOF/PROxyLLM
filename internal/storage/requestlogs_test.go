package storage

import (
	"context"
	"encoding/json"
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
