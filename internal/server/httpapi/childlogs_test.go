package httpapi

// Sub-operations as their own rows (Logs page): a request whose
// trace recorded a failover must persist a child row for it, linked to the
// request's row id, so the fallback is visible in the log rather than only
// inside the parent's route_trace. Spec: the operator report that the
// vision-fallback and web requests do not appear in the log; the mapping
// itself is pinned in internal/routing/childops_test.go.

import (
	"context"
	"net/http"
	"testing"

	"github.com/tubeof/proxyllm/internal/routing"
)

// TestRequestLog_PersistsFailoverChildRow: a two-entry chain whose first
// attempt fails with 500 and whose second serves 200 records exactly one
// failover transition — and that transition must exist as a child row under
// the served request's id.
func TestRequestLog_PersistsFailoverChildRow(t *testing.T) {
	e := newEnvWithRules(t, routing.Rule{
		MatchModel: "gpt-5",
		Chain:      []string{"mock-main", "mock-main"},
		Priority:   100,
		Enabled:    true,
	})
	e.mock.Enqueue(mockFail(t, 500, `{"error":{"message":"boom","type":"server_error","code":null}}`))
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))

	status, body := e.post(t, "/v1/chat/completions", string(golden(t, "openai/chat-request.json")))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}

	logs := e.requestLogs(t)
	if len(logs) != 1 {
		t.Fatalf("request rows = %d, want 1", len(logs))
	}
	parent := logs[0].ID

	children, err := e.store.ListChildLogsForParents(context.Background(), []int64{parent})
	if err != nil {
		t.Fatalf("ListChildLogsForParents: %v", err)
	}
	ops := children[parent]
	if len(ops) != 1 {
		t.Fatalf("child rows = %d, want 1 failover op: %+v", len(ops), ops)
	}
	if ops[0].Kind != string(routing.OpFailover) {
		t.Errorf("child kind = %q, want %q", ops[0].Kind, routing.OpFailover)
	}
	if ops[0].ParentID != parent {
		t.Errorf("child parent = %d, want the served request row %d", ops[0].ParentID, parent)
	}
}

// TestRequestLog_CleanRequestHasNoChildRows: a request that served through
// its primary on the first try must persist no child rows — the ordinary
// case stays a single row.
func TestRequestLog_CleanRequestHasNoChildRows(t *testing.T) {
	e := newEnv(t)
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))

	status, body := e.post(t, "/v1/chat/completions", string(golden(t, "openai/chat-request.json")))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}

	logs := e.requestLogs(t)
	if len(logs) != 1 {
		t.Fatalf("request rows = %d, want 1", len(logs))
	}
	children, err := e.store.ListChildLogsForParents(context.Background(), []int64{logs[0].ID})
	if err != nil {
		t.Fatalf("ListChildLogsForParents: %v", err)
	}
	if len(children[logs[0].ID]) != 0 {
		t.Errorf("child rows = %+v, want none for a clean request", children[logs[0].ID])
	}
}
