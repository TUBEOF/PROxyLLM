package server

// End-to-end, through a fully assembled server (Logs page): an
// emulated web_search must end with a child row whose OWN payload an operator
// can open — the query the step sent and the result it got back — and that
// result body must NOT be in route_trace.
//
// Both halves matter. The first is the operator's report: a web-tool sub-row was
// a dead line in the log. The second is structural: route_trace is a versioned
// wire schema the WebUI renders, it stays metadata, and nothing redacts it on
// the way into the database.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/testutil/mockprovider"
	"github.com/tubeof/proxyllm/internal/tools/firecrawl"
)

func TestServer_EmulatedToolChildRowCarriesItsOwnPayload(t *testing.T) {
	ctx := context.Background()
	cfg := newConfig(t)

	st, err := storage.Open(cfg.Storage)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	if err := st.EnsureProvider(ctx, "mock-main", "custom", "Mock Main"); err != nil {
		t.Fatalf("ensure provider: %v", err)
	}
	if err := st.PutRoutingRule(ctx, storage.RoutingRule{
		ID: "r1", MatchModel: "gpt-5", Chain: []string{"mock-main"}, Priority: 100, Enabled: true,
	}); err != nil {
		t.Fatalf("put rule: %v", err)
	}
	if err := st.PutModel(ctx, storage.Model{
		ID: "gpt-5", ProviderID: "mock-main", Name: "gpt-5",
		Compat: json.RawMessage(`{"server_tool_emulation":"emulate-firecrawl"}`),
	}); err != nil {
		t.Fatalf("put model: %v", err)
	}

	// Firecrawl-shaped upstream: one result whose text is distinctive enough to
	// recognise anywhere it turns up — including in a place it must not.
	fc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"web":[{"title":"Go","url":"https://go.dev/","description":"the Go programming language"}]}}`))
	}))
	defer fc.Close()

	mock := mockprovider.New(t)
	srv, err := New(&cfg, st, nil,
		WithProvider(mock.Provider("mock-main", providerAllCaps())),
		WithEmulator(firecrawl.NewEmulator(fc.URL, "fc-key", nil)),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mock.Enqueue(mockprovider.NewScript().JSONResponse(http.StatusOK,
		`{"id":"c1","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"go\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil))
	mock.Enqueue(mockprovider.NewScript().JSONResponse(http.StatusOK,
		`{"id":"c2","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil))

	_, key, err := authCreateKey(ctx, st)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages",
		strings.NewReader(`{"model":"gpt-5","max_tokens":128,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"search go"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.ReadAll(resp.Body)

	logs, err := st.ListRequestLogs(ctx, 10)
	if err != nil {
		t.Fatalf("ListRequestLogs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("request rows = %d, want 1", len(logs))
	}
	row := logs[0]

	byParent, err := st.ListChildLogsForParents(ctx, []int64{row.ID})
	if err != nil {
		t.Fatalf("ListChildLogsForParents: %v", err)
	}
	ops := byParent[row.ID]
	if len(ops) != 1 || ops[0].Kind != string(routing.OpEmulatedTool) {
		t.Fatalf("child rows = %+v, want exactly one emulated_tool op", ops)
	}
	op := ops[0]

	// The payload write is deliberately off the request path, so it lands a
	// moment after the response — poll rather than race it.
	var payload storage.RequestPayload
	found := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if payload, found, err = st.GetRequestPayload(ctx, storage.PayloadOwnerChild, op.ID); err != nil {
			t.Fatalf("GetRequestPayload(child): %v", err)
		}
		if found {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !found {
		t.Fatalf("no payload stored for child row %d — the row opens an empty detail view", op.ID)
	}

	// What the step sent and what it got back.
	if !strings.Contains(payload.ClientRequest, "web_search") || !strings.Contains(payload.ClientRequest, "go") {
		t.Errorf("child payload request = %q, want the tool call the step answered", payload.ClientRequest)
	}
	if !strings.Contains(payload.ClientResponse, "go.dev") {
		t.Errorf("child payload response = %q, want the result the model was handed", payload.ClientResponse)
	}

	// And the structural rule: the body never enters the versioned trace.
	if strings.Contains(string(row.RouteTrace), "go.dev") {
		t.Error("route_trace carries the tool result body — the trace stays metadata")
	}
}

// A request with no sub-operation must store no child payload: an ordinary
// request's log row is the common case and costs nothing.
func TestServer_PlainRequestStoresNoChildPayload(t *testing.T) {
	ctx := context.Background()
	cfg := newConfig(t)
	st, err := storage.Open(cfg.Storage)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	if err := st.EnsureProvider(ctx, "mock-main", "custom", "Mock Main"); err != nil {
		t.Fatalf("ensure provider: %v", err)
	}
	if err := st.PutRoutingRule(ctx, storage.RoutingRule{
		ID: "r1", MatchModel: "gpt-5", Chain: []string{"mock-main"}, Priority: 100, Enabled: true,
	}); err != nil {
		t.Fatalf("put rule: %v", err)
	}

	mock := mockprovider.New(t)
	srv, err := New(&cfg, st, nil, WithProvider(mock.Provider("mock-main", providerAllCaps())))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mock.Enqueue(mockprovider.NewScript().JSONResponse(http.StatusOK,
		`{"id":"c1","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil))

	_, key, err := authCreateKey(ctx, st)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages",
		strings.NewReader(`{"model":"gpt-5","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.ReadAll(resp.Body)

	logs, err := st.ListRequestLogs(ctx, 10)
	if err != nil {
		t.Fatalf("ListRequestLogs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("request rows = %d, want 1", len(logs))
	}
	byParent, err := st.ListChildLogsForParents(ctx, []int64{logs[0].ID})
	if err != nil {
		t.Fatalf("ListChildLogsForParents: %v", err)
	}
	if ops := byParent[logs[0].ID]; len(ops) != 0 {
		t.Errorf("child rows = %+v, want none for a plain request", ops)
	}
}
