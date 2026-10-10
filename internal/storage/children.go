package storage

import (
	"context"
	"strings"
)

// ChildLog is one persisted sub-operation of a request (Logs page,
// 2026-10-07): a chain failover, a capability (vision) fallback model walk,
// or an emulated web-tool call. It is derived from the parent request's
// route_trace by routing.ChildOps and stored beside it so the operation is
// visible as its own row (a same-provider retry is deliberately NOT a row
// — it is one attempt of the same operation, and a retry storm would flood
// the log; it stays a timeline event).
//
// It carries provider/model identity and an operator-facing detail string
// only — never request bodies, headers, or credentials. Detail is
// upstream-influenced for the emulation case and is masked by the write
// path (httpapi.ChildOpsFromTrace) before it reaches this table.
type ChildLog struct {
	ID       int64
	ParentID int64
	TS       int64

	// Kind is the routing.OpKind string ("failover", "capability_fallback",
	// "emulated_tool").
	Kind string

	ProviderID string
	Model      string

	// FromRef/ToRef carry a transition's endpoints (provider or account
	// identity for failover, the tool name in ToRef for emulated_tool).
	FromRef string
	ToRef   string

	Detail string
}

// InsertChildLogs appends a request's sub-operation rows, all under one
// parent request-log id, and returns the ids it allocated in insertion order.
// A no-op (no INSERT) when ops is empty, so the common case — a request with
// no failover/emulation/retry — costs nothing.
//
// The ids come back because a sub-operation's own payload is addressed by its
// row id (request_payloads owner_kind "child"): a detail view needs to know
// which row it is showing, and deriving that from a second query would re-read
// what this call already knows.
func (s *Store) InsertChildLogs(ctx context.Context, parentID int64, ops []ChildLog) ([]int64, error) {
	ids := make([]int64, 0, len(ops))
	if len(ops) == 0 {
		return ids, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// RETURNING id (SQLite ≥ 3.35) rather than LastInsertId: one statement per
	// op either way, and the allocated id arrives with the row it belongs to
	// instead of being read back off a shared connection (wrong if anything
	// else wrote in between).
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO request_child_logs (parent_id, ts, kind, provider_id, model, from_ref, to_ref, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stmt.Close() }()

	for _, op := range ops {
		var id int64
		if err := stmt.QueryRowContext(ctx, parentID, op.TS, op.Kind,
			op.ProviderID, op.Model, op.FromRef, op.ToRef, op.Detail).Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// ListChildLogsForParents returns each parent's sub-operations, keyed by
// parent id, in insertion order. One query for a whole page of parents —
// never one query per row. An empty parent list issues no query (SQLite has
// no empty IN list), and a parent with no children is simply absent from
// the map.
func (s *Store) ListChildLogsForParents(ctx context.Context, parentIDs []int64) (map[int64][]ChildLog, error) {
	out := map[int64][]ChildLog{}
	if len(parentIDs) == 0 {
		return out, nil
	}

	// Placeholders are generated from the arg count only — no user text
	// enters the SQL string.
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(parentIDs)), ",")
	q := `SELECT id, parent_id, ts, kind, provider_id, model, from_ref, to_ref, detail FROM request_child_logs WHERE parent_id IN (` + placeholders + `) ORDER BY id` //nolint:gosec // placeholders are fixed literals; values stay bound args
	args := make([]any, 0, len(parentIDs))
	for _, id := range parentIDs {
		args = append(args, id)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var c ChildLog
		var providerID, model, fromRef, toRef, detail string
		if err := rows.Scan(&c.ID, &c.ParentID, &c.TS, &c.Kind, &providerID,
			&model, &fromRef, &toRef, &detail); err != nil {
			return nil, err
		}
		c.ProviderID, c.Model = providerID, model
		c.FromRef, c.ToRef, c.Detail = fromRef, toRef, detail
		out[c.ParentID] = append(out[c.ParentID], c)
	}
	return out, rows.Err()
}
