package storage

import (
	"context"
	"testing"
)

// Spec: the Logs page shows a request's sub-operations as their own rows
// (2026-10-07). They live in their own table so request_logs keeps its
// current shape and semantics — every row there is a request, so the page's
// pagination and its `total` count are unaffected. The parent link is the
// request_logs row id.

// seedRequestLog inserts one request_logs row and returns its id.
func seedRequestLog(t *testing.T, s *Store, ts int64) int64 {
	t.Helper()
	id, err := s.InsertRequestLog(context.Background(), RequestLog{
		Timestamp: ts, Model: "glm-5.3", Status: 200,
	})
	if err != nil {
		t.Fatalf("InsertRequestLog: %v", err)
	}
	return id
}

func TestChildLog_InsertAndReadBack(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()
	parent := seedRequestLog(t, s, 1760000000)

	ops := []ChildLog{
		{Kind: "failover", FromRef: "acc-a", ToRef: "acc-b", Detail: "chain failover after rate_limit", TS: 1760000001},
		{Kind: "capability_fallback", Detail: "vision", TS: 1760000002},
		{Kind: "emulated_tool", ToRef: "web_search", Detail: `query "golang" → 5 result(s)`, TS: 1760000003},
		{Kind: "retry", ProviderID: "acc-b", Model: "glm-5.3", Detail: "retry after 503", TS: 1760000004},
	}
	if err := s.InsertChildLogs(ctx, parent, ops); err != nil {
		t.Fatalf("InsertChildLogs: %v", err)
	}

	byParent, err := s.ListChildLogsForParents(ctx, []int64{parent})
	if err != nil {
		t.Fatalf("ListChildLogsForParents: %v", err)
	}
	got := byParent[parent]
	if len(got) != len(ops) {
		t.Fatalf("children = %d, want %d", len(got), len(ops))
	}
	for i := range ops {
		if got[i].ID <= 0 {
			t.Errorf("child[%d].ID = %d, want a positive autoincrement id", i, got[i].ID)
		}
		if got[i].ParentID != parent {
			t.Errorf("child[%d].ParentID = %d, want %d", i, got[i].ParentID, parent)
		}
		if got[i].Kind != ops[i].Kind || got[i].FromRef != ops[i].FromRef ||
			got[i].ToRef != ops[i].ToRef || got[i].Detail != ops[i].Detail ||
			got[i].ProviderID != ops[i].ProviderID || got[i].Model != ops[i].Model ||
			got[i].TS != ops[i].TS {
			t.Errorf("child[%d] round-trip mismatch:\n got %+v\nwant %+v", i, got[i], ops[i])
		}
	}
}

// TestChildLog_OrderedByInsertion: the frontend renders the ops as a
// sequence, so the read order must follow insertion order even when two ops
// share a timestamp.
func TestChildLog_OrderedByInsertion(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()
	parent := seedRequestLog(t, s, 1760000000)

	if err := s.InsertChildLogs(ctx, parent, []ChildLog{
		{Kind: "failover", Detail: "first", TS: 1760000001},
		{Kind: "retry", Detail: "second", TS: 1760000001},
		{Kind: "emulated_tool", Detail: "third", TS: 1760000001},
	}); err != nil {
		t.Fatalf("InsertChildLogs: %v", err)
	}

	got := mustChildren(t, s, parent)
	for i, want := range []string{"first", "second", "third"} {
		if got[i].Detail != want {
			t.Errorf("child[%d].Detail = %q, want %q (insertion order lost)", i, got[i].Detail, want)
		}
	}
}

// TestChildLog_BatchedReadForManyParents: one page of the log viewer asks
// for every parent's children at once — never one query per row.
func TestChildLog_BatchedReadForManyParents(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()
	p1 := seedRequestLog(t, s, 1760000100)
	p2 := seedRequestLog(t, s, 1760000200)
	p3 := seedRequestLog(t, s, 1760000300)

	if err := s.InsertChildLogs(ctx, p1, []ChildLog{{Kind: "failover", Detail: "p1 a"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertChildLogs(ctx, p2, []ChildLog{
		{Kind: "retry", Detail: "p2 a"}, {Kind: "retry", Detail: "p2 b"},
	}); err != nil {
		t.Fatal(err)
	}
	// p3 deliberately has no children.

	byParent, err := s.ListChildLogsForParents(ctx, []int64{p1, p2, p3})
	if err != nil {
		t.Fatalf("ListChildLogsForParents: %v", err)
	}
	if len(byParent[p1]) != 1 || len(byParent[p2]) != 2 {
		t.Errorf("counts = %d/%d, want 1/2", len(byParent[p1]), len(byParent[p2]))
	}
	if _, ok := byParent[p3]; ok {
		t.Errorf("parent with no children must be absent from the map, got %+v", byParent[p3])
	}
}

// TestChildLog_EmptyParentListIsNoQuery: an empty page (or a page whose rows
// were all filtered out) must not issue a query with an empty IN list.
func TestChildLog_EmptyParentListIsNoQuery(t *testing.T) {
	s := openTokenStore(t)
	byParent, err := s.ListChildLogsForParents(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListChildLogsForParents(nil): %v", err)
	}
	if len(byParent) != 0 {
		t.Errorf("byParent = %+v, want empty", byParent)
	}
}

// TestChildLog_AbsentFromRequestLogsPage is the contract that keeps the
// pager honest: sub-operations are not requests. They must never appear in
// ListRequestLogsPage nor in its total.
func TestChildLog_AbsentFromRequestLogsPage(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()
	parent := seedRequestLog(t, s, 1760000000)
	if err := s.InsertChildLogs(ctx, parent, []ChildLog{
		{Kind: "failover", Detail: "a"}, {Kind: "emulated_tool", Detail: "b"},
	}); err != nil {
		t.Fatal(err)
	}

	rows, total, err := s.ListRequestLogsPage(ctx, RequestLogFilter{}, 50, 0)
	if err != nil {
		t.Fatalf("ListRequestLogsPage: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Errorf("page = %d rows, total %d, want 1/1 (children must not count as requests)", len(rows), total)
	}
	if rows[0].ID != parent {
		t.Errorf("page row id = %d, want the request row %d", rows[0].ID, parent)
	}
}

// TestChildLog_FilteredParentStillGetsChildren: the map is keyed by the
// parent id, not by the page position — a page containing a subset of
// parents still carries each parent's children.
func TestChildLog_FilteredParentStillGetsChildren(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()
	pOld := seedRequestLog(t, s, 1760000000)
	pNew := seedRequestLog(t, s, 1760000900)
	if err := s.InsertChildLogs(ctx, pOld, []ChildLog{{Kind: "retry", Detail: "old"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertChildLogs(ctx, pNew, []ChildLog{{Kind: "retry", Detail: "new"}}); err != nil {
		t.Fatal(err)
	}

	rows, _, err := s.ListRequestLogsPage(ctx, RequestLogFilter{Since: 1760000500}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	byParent, err := s.ListChildLogsForParents(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(byParent[pNew]) != 1 || byParent[pNew][0].Detail != "new" {
		t.Errorf("filtered page children = %+v, want the new parent's one child", byParent[pNew])
	}
	if _, ok := byParent[pOld]; ok {
		t.Errorf("out-of-range parent must not appear: %+v", byParent[pOld])
	}
}

func mustChildren(t *testing.T, s *Store, parent int64) []ChildLog {
	t.Helper()
	byParent, err := s.ListChildLogsForParents(context.Background(), []int64{parent})
	if err != nil {
		t.Fatalf("ListChildLogsForParents: %v", err)
	}
	return byParent[parent]
}
