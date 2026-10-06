package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	// Logs-page redesign (v5): conversation identity and surface meta.
	// ConversationID derives only for post-patch rows; ConvKey is the
	// internal grouping input (never exposed on the wire).
	ConversationID string
	ConvKey        string
	Protocol       string
	Features       json.RawMessage
}

// requestLogCols is the column list shared by every request_logs lister —
// the SELECT projection and the Scan target must stay in lockstep.
const requestLogCols = `id, ts, client_key_id, provider_id, model, status, latency_ms, usage, route_trace, error_redacted, conversation_id, conv_key, protocol, features`

// scanRequestLog scans one request_logs row in requestLogCols order.
func scanRequestLog(rows *sql.Rows) (RequestLog, error) {
	var l RequestLog
	var clientKeyID, providerID, model, errorRedacted sql.NullString
	var conversationID, convKey, protocol sql.NullString
	var usage, trace, features string
	if err := rows.Scan(&l.ID, &l.Timestamp, &clientKeyID, &providerID, &model,
		&l.Status, &l.LatencyMS, &usage, &trace, &errorRedacted,
		&conversationID, &convKey, &protocol, &features); err != nil {
		return RequestLog{}, err
	}
	l.ClientKeyID = clientKeyID.String
	l.ProviderID = providerID.String
	l.Model = model.String
	l.ErrorRedacted = errorRedacted.String
	l.ConversationID = conversationID.String
	l.ConvKey = convKey.String
	l.Protocol = protocol.String
	l.Usage = json.RawMessage(usage)
	l.RouteTrace = json.RawMessage(trace)
	l.Features = json.RawMessage(features)
	return l, nil
}

// scanRequestLogs drains the rows in requestLogCols order.
func scanRequestLogs(rows *sql.Rows) ([]RequestLog, error) {
	defer func() { _ = rows.Close() }()
	var out []RequestLog
	for rows.Next() {
		l, err := scanRequestLog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// InsertRequestLog appends one log row and returns the autoincrement id.
// Empty usage/route_trace/features default to '{}' — a log row must always
// render valid JSON in the WebUI, never NULL.
func (s *Store) InsertRequestLog(ctx context.Context, l RequestLog) (int64, error) {
	if len(l.Usage) == 0 {
		l.Usage = json.RawMessage("{}")
	}
	if len(l.RouteTrace) == 0 {
		l.RouteTrace = json.RawMessage("{}")
	}
	if len(l.Features) == 0 {
		l.Features = json.RawMessage("{}")
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO request_logs (ts, client_key_id, provider_id, model, status, latency_ms, usage, route_trace, error_redacted, conversation_id, conv_key, protocol, features)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.Timestamp, l.ClientKeyID, l.ProviderID, l.Model, l.Status, l.LatencyMS,
		string(l.Usage), string(l.RouteTrace), l.ErrorRedacted,
		l.ConversationID, l.ConvKey, l.Protocol, string(l.Features))
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
		SELECT `+requestLogCols+`
		FROM request_logs
		ORDER BY ts DESC, id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	return scanRequestLogs(rows)
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

// ListRequestLogsAfter returns rows with id > after (the live-feed
// cursor: the page polls its last-seen id for newer rows), newest-first,
// limited. Filter semantics match the page query.
func (s *Store) ListRequestLogsAfter(ctx context.Context, f RequestLogFilter, limit int, after int64) ([]RequestLog, error) {
	if limit <= 0 {
		limit = 50
	}
	where, args := f.whereClause()
	if where == "" {
		where = " WHERE id > ?"
		args = []any{after}
	} else {
		where += " AND id > ?"
		args = append(args, after)
	}
	q := `SELECT ` + requestLogCols + ` FROM request_logs` + where + ` ORDER BY id DESC LIMIT ?` //nolint:gosec // where is built from fixed literals only
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return scanRequestLogs(rows)
}

// RecentConversation returns the conversation_id of the newest row with the
// same client key and conv_key whose ts is within `within` seconds of `ts`
// ("" when none): the rolling-window conversation assignment of the
// Logs-page redesign. The window keeps "same system prompt, different
// conversation a week later" from merging forever without any wall-clock
// state in the render path.
func (s *Store) RecentConversation(ctx context.Context, clientKeyID, convKey string, ts, within int64) (string, error) {
	var convID sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT conversation_id
		FROM request_logs
		WHERE client_key_id = ? AND conv_key = ? AND conversation_id != ''
			AND ts >= ? AND ts <= ?
		ORDER BY id DESC
		LIMIT 1`,
		clientKeyID, convKey, ts-within, ts).Scan(&convID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return convID.String, nil
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
	q := `SELECT ` + requestLogCols + ` FROM request_logs` + where + ` ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?` //nolint:gosec // where is built from fixed literals only
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	logs, err := scanRequestLogs(rows)
	if err != nil {
		return nil, 0, err
	}
	return logs, total, nil
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
