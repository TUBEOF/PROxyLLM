package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
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

// RequestLogFilter selects request-log rows for the WebUI log viewer. The
// zero value matches everything; filters AND together. Status/…Since/Until
// of 0 mean "no constraint" (timestamps are unix seconds).
type RequestLogFilter struct {
	Model      string
	ProviderID string
	Status     int
	Since      int64
	Until      int64
}

// ListRequestLogsPage returns one page of newest-first rows plus the total
// number of matching rows (so the UI can render page links without a second
// query per page). limit<=0 defaults to 50; offset is 0-based; an offset
// past the end is an empty page, not an error.
func (s *Store) ListRequestLogsPage(ctx context.Context, f RequestLogFilter, limit, offset int) ([]RequestLog, int, error) {
	if limit <= 0 {
		limit = 50
	}

	where, args := f.whereClause()
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM request_logs`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// whereClause emits only fixed literals (values stay placeholders), so
	// no user-controlled text can enter the SQL string.
	const logCols = `id, ts, client_key_id, provider_id, model, status, latency_ms, usage, route_trace, error_redacted`
	q := `SELECT ` + logCols + ` FROM request_logs` + where + ` ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?` //nolint:gosec // where is built from fixed literals only
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var out []RequestLog
	for rows.Next() {
		var l RequestLog
		var clientKeyID, providerID, model, errorRedacted sql.NullString
		var usage, trace string
		if err := rows.Scan(&l.ID, &l.Timestamp, &clientKeyID, &providerID, &model,
			&l.Status, &l.LatencyMS, &usage, &trace, &errorRedacted); err != nil {
			return nil, 0, err
		}
		l.ClientKeyID = clientKeyID.String
		l.ProviderID = providerID.String
		l.Model = model.String
		l.ErrorRedacted = errorRedacted.String
		l.Usage = json.RawMessage(usage)
		l.RouteTrace = json.RawMessage(trace)
		out = append(out, l)
	}
	return out, total, rows.Err()
}

// whereClause renders the filter as a SQL WHERE prefix plus positional
// args. Zero-valued fields contribute nothing (they mean "no constraint").
func (f RequestLogFilter) whereClause() (string, []any) {
	var conds []string
	var args []any
	if f.Model != "" {
		conds = append(conds, "model = ?")
		args = append(args, f.Model)
	}
	if f.ProviderID != "" {
		conds = append(conds, "provider_id = ?")
		args = append(args, f.ProviderID)
	}
	if f.Status != 0 {
		conds = append(conds, "status = ?")
		args = append(args, f.Status)
	}
	if f.Since != 0 {
		conds = append(conds, "ts >= ?")
		args = append(args, f.Since)
	}
	if f.Until != 0 {
		conds = append(conds, "ts <= ?")
		args = append(args, f.Until)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}
