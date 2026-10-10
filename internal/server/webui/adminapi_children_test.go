package webui

// Sub-operations on the log page response (2026-10-07): each log row ships
// its child operations keyed by row id, in one batched read — the WebUI
// renders a fallback model walk or an emulated web-tool call as its own
// nested row without a second round trip per row.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tubeof/proxyllm/internal/storage"
)

func TestLogs_ChildrenKeyedByParentID(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")

	p1 := seedLogWithChildren(t, e, 1, []storage.ChildLog{
		{Kind: "failover", FromRef: "acc-a", ToRef: "acc-b", Detail: "chain failover after rate_limit"},
		{Kind: "emulated_tool", ToRef: "web_search", Detail: `query "golang" → 5 result(s)`},
	})
	p2 := seedLogWithChildren(t, e, 2, nil) // no children
	p3 := seedLogWithChildren(t, e, 3, []storage.ChildLog{
		{Kind: "capability_fallback", Detail: "vision"},
	})

	var res struct {
		Logs     []logDTO `json:"logs"`
		Total    int      `json:"total"`
		Children map[string][]struct {
			ID         int64  `json:"id"`
			ParentID   int64  `json:"parent_id"`
			Kind       string `json:"kind"`
			ProviderID string `json:"provider_id,omitempty"`
			Model      string `json:"model,omitempty"`
			FromRef    string `json:"from_ref,omitempty"`
			ToRef      string `json:"to_ref,omitempty"`
			Detail     string `json:"detail,omitempty"`
		} `json:"children"`
	}
	w := e.authedCall(t, http.MethodGet, "/api/admin/logs?limit=50", "", s)
	assertStatus(t, w, http.StatusOK, "logs with children")
	decodeJSON(t, w, &res)

	if res.Total != 3 {
		t.Errorf("total = %d, want 3 (children must not count as requests)", res.Total)
	}
	key := func(id int64) string {
		b, _ := json.Marshal(id)
		return string(b)
	}
	if got := res.Children[key(p1)]; len(got) != 2 {
		t.Fatalf("children[%d] = %+v, want 2 ops", p1, got)
	}
	if res.Children[key(p1)][0].Kind != "failover" || res.Children[key(p1)][0].ParentID != p1 {
		t.Errorf("first child = %+v, want a failover op under %d", res.Children[key(p1)][0], p1)
	}
	if res.Children[key(p1)][1].ToRef != "web_search" {
		t.Errorf("second child = "+"%+v, want the emulated web_search op", res.Children[key(p1)][1])
	}
	if _, present := res.Children[key(p2)]; present {
		t.Errorf("parent %d has no children and must be absent: %+v", p2, res.Children[key(p2)])
	}
	if got := res.Children[key(p3)]; len(got) != 1 || got[0].Detail != "vision" {
		t.Errorf("children[%d] = %+v, want one capability_fallback op with detail vision", p3, got)
	}
}

// TestLogs_NoChildrenOmitsField: with no sub-operations anywhere, the
// field is absent (omitempty) — the common page stays exactly as before.
func TestLogs_NoChildrenOmitsField(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")
	if _, err := e.st.InsertRequestLog(context.Background(), testLogRow(1)); err != nil {
		t.Fatal(err)
	}

	var raw map[string]json.RawMessage
	w := e.authedCall(t, http.MethodGet, "/api/admin/logs?limit=50", "", s)
	assertStatus(t, w, http.StatusOK, "logs without children")
	decodeJSON(t, w, &raw)
	if _, present := raw["children"]; present {
		t.Errorf("children field present with no children: %s", raw["children"])
	}
}

// seedLogWithChildren inserts one request row plus its child ops and
// returns the parent row id.
func seedLogWithChildren(t *testing.T, e *adminEnv, i int, ops []storage.ChildLog) int64 {
	t.Helper()
	ctx := context.Background()
	parent, err := e.st.InsertRequestLog(ctx, testLogRow(i))
	if err != nil {
		t.Fatalf("insert parent: %v", err)
	}
	if _, err := e.st.InsertChildLogs(ctx, parent, ops); err != nil {
		t.Fatalf("insert children: %v", err)
	}
	return parent
}
