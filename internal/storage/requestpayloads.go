package storage

// Request payloads (Logs page): the conversation a request carried
// and the answer it produced. Without them the log is metadata only — status,
// tokens, provider, route_trace — which answers "what happened" but not "what
// was actually said": an operator debugging a routing decision could see
// that a fallback fired but not what the fallback model answered.
//
// Two shape decisions matter here:
//
//   - A table of its own rather than columns on request_logs. The list query
//     runs on every Logs-page poll and must stay cheap; dragging prompt text
//     out of disk with it would tax the common case to serve the rare one.
//   - Capped and redacted before the write. Captured prompts are the one place
//     this store holds text a client chose, so size is bounded by a constant
//     and secrets are the WRITE PATH's responsibility (httpapi redacts through
//     internal/observability/redact before calling in — the same redactor every
//     other log site uses). Storage stores what it is given.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// Owner kinds for a payload row.
const (
	// PayloadOwnerRequest: the payload belongs to a request_logs row.
	PayloadOwnerRequest = "request"

	// PayloadOwnerChild: the payload belongs to a request_child_logs row.
	PayloadOwnerChild = "child"
)

// MaxPayloadBytes caps one stored payload. A prompt past this is stored as a
// marked prefix rather than in full: the useful diagnostic (the start of the
// conversation, the first screens of a tool result) is almost always near the
// front, and an uncapped column turns one long agentic session into a
// multi-megabyte row on every request.
const MaxPayloadBytes = 64 * 1024

// RequestPayload is one log row's captured conversation and answer.
type RequestPayload struct {
	OwnerKind string
	OwnerID   int64
	TS        int64

	// ClientRequest is the request's messages (and system prompt) as JSON,
	// redacted and capped. ClientResponse is the answer the client received.
	ClientRequest  string
	ClientResponse string

	// Bytes is the stored size (both fields), Truncated whether either was cut.
	Bytes     int64
	Truncated bool
}

// CapPayload bounds s to MaxPayloadBytes. An over-long value comes back as a
// JSON envelope naming the original size and carrying a prefix, so a reader
// can tell a short conversation from a cut-off one instead of silently
// misreading the truncation as the whole thing.
func CapPayload(s string) (string, bool) {
	if len(s) <= MaxPayloadBytes {
		return s, false
	}
	// The preview is JSON-escaped by Marshal, so the envelope stays valid JSON
	// even when the cut lands mid-escape-sequence.
	preview, err := json.Marshal(s[:MaxPayloadBytes])
	if err != nil {
		return `{"_truncated":true}`, true
	}
	return `{"_truncated":true,"_originalSize":` + strconv.Itoa(len(s)) +
		`,"_preview":` + string(preview) + `}`, true
}

// PutRequestPayload upserts one row's payload. An empty payload is not stored
// (nothing to show, no row to prune).
func (s *Store) PutRequestPayload(ctx context.Context, p RequestPayload) error {
	if p.ClientRequest == "" && p.ClientResponse == "" {
		return nil
	}
	if p.OwnerKind != PayloadOwnerRequest && p.OwnerKind != PayloadOwnerChild {
		return fmt.Errorf("storage: unknown payload owner kind %q", p.OwnerKind)
	}
	req, reqCut := CapPayload(p.ClientRequest)
	resp, respCut := CapPayload(p.ClientResponse)
	var total int64
	if req != "" {
		total += int64(len(req))
	}
	if resp != "" {
		total += int64(len(resp))
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO request_payloads
			(owner_kind, owner_id, ts, client_request, client_response, payload_bytes, truncated)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(owner_kind, owner_id) DO UPDATE SET
			ts = excluded.ts,
			client_request = excluded.client_request,
			client_response = excluded.client_response,
			payload_bytes = excluded.payload_bytes,
			truncated = excluded.truncated`,
		p.OwnerKind, p.OwnerID, p.TS, nullIfEmpty(req), nullIfEmpty(resp), total, boolToInt(reqCut || respCut))
	if err != nil {
		return fmt.Errorf("storage: put request payload: %w", err)
	}
	return nil
}

// GetRequestPayload reads one row's payload. found is false when none was
// stored (capture disabled, or the row predates the feature).
func (s *Store) GetRequestPayload(ctx context.Context, ownerKind string, ownerID int64) (RequestPayload, bool, error) {
	var p RequestPayload
	var req, resp sql.NullString
	var truncated int
	err := s.db.QueryRowContext(ctx, `
		SELECT owner_kind, owner_id, ts, client_request, client_response, payload_bytes, truncated
		FROM request_payloads WHERE owner_kind = ? AND owner_id = ?`,
		ownerKind, ownerID).
		Scan(&p.OwnerKind, &p.OwnerID, &p.TS, &req, &resp, &p.Bytes, &truncated)
	if errors.Is(err, sql.ErrNoRows) {
		return RequestPayload{}, false, nil
	}
	if err != nil {
		return RequestPayload{}, false, fmt.Errorf("storage: get request payload: %w", err)
	}
	p.ClientRequest = req.String
	p.ClientResponse = resp.String
	p.Truncated = truncated != 0
	return p, true, nil
}

// PruneRequestPayloads deletes payloads older than the given unix second and
// reports how many rows went. It never touches request_logs: the metadata row
// is the record that a request happened, and it must outlive the conversation
// that was captured beside it.
func (s *Store) PruneRequestPayloads(ctx context.Context, olderThan int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM request_payloads WHERE ts < ?`, olderThan)
	if err != nil {
		return 0, fmt.Errorf("storage: prune request payloads: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil // the delete succeeded; a driver that cannot count is not an error
	}
	return n, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
