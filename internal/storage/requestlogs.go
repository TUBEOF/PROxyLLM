package storage

import (
	"context"
	"database/sql"
	"encoding/json"
)

// RequestLog is one persisted request log row (plan Analysis C schema +
// Analysis E FO-4 + Analysis G): route_trace keeps every observable routing
// decision, error_redacted is the ONLY error text that ever reaches this
// table — both feeding the WebUI log viewer.
type RequestLog struct {
	ID            int64
	Timestamp     int64
	ClientKeyID   string
	ProviderID    string
	Model         string
	Status        int
	LatencyMS     int64
	Usage         json.RawMessage
	RouteTrace    json.RawMessage
	ErrorRedacted string
}

// InsertRequestLog appends one log row and returns the autoincrement id.
// Empty usage/route_trace default to '{}' — a log row must always render
// valid JSON in the WebUI, never NULL.
func (s *Store) InsertRequestLog(ctx context.Context, l RequestLog) (int64, error) {
	if len(l.Usage) == 0 {
		l.Usage = json.RawMessage("{}")
	}
	if len(l.RouteTrace) == 0 {
		l.RouteTrace = json.RawMessage("{}")
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO request_logs (ts, client_key_id, provider_id, model, status, latency_ms, usage, route_trace, error_redacted)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.Timestamp, l.ClientKeyID, l.ProviderID, l.Model, l.Status, l.LatencyMS,
		string(l.Usage), string(l.RouteTrace), l.ErrorRedacted)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListRequestLogs returns the newest limit rows (WebUI log viewer): ts
// descending, id descending as the tie-break so equal timestamps keep a
// stable insertion order.
func (s *Store) ListRequestLogs(ctx context.Context, limit int) ([]RequestLog, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, ts, client_key_id, provider_id, model, status, latency_ms, usage, route_trace, error_redacted
		FROM request_logs
		ORDER BY ts DESC, id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []RequestLog
	for rows.Next() {
		var l RequestLog
		var clientKeyID, providerID, model, errorRedacted sql.NullString
		var usage, trace string
		if err := rows.Scan(&l.ID, &l.Timestamp, &clientKeyID, &providerID, &model,
			&l.Status, &l.LatencyMS, &usage, &trace, &errorRedacted); err != nil {
			return nil, err
		}
		l.ClientKeyID = clientKeyID.String
		l.ProviderID = providerID.String
		l.Model = model.String
		l.ErrorRedacted = errorRedacted.String
		l.Usage = json.RawMessage(usage)
		l.RouteTrace = json.RawMessage(trace)
		out = append(out, l)
	}
	return out, rows.Err()
}
