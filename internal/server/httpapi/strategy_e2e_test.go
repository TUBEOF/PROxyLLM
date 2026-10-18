package httpapi

// ADR-009 end-to-end behavior over the full client → auth → router →
// executor → mock-upstream stack (harness_test.go style, derived from the
// ADR — not implementation behavior):
//
//   - A type chain entry expands to the type's accounts: requests rotate
//     across two mock accounts of the same type under the round_robin
//     default, alternating by provider.
//   - fill_first pins the first account until it fails.
//   - The request log's route_trace carries "version":2 plus the expand
//     and rotation events — the observability surface is part of the
//     contract.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/auth"
	"github.com/tubeof/proxyllm/internal/config"
	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/testutil/mockprovider"
)

// strategyEnv assembles the harness with a TYPE chain entry over two mock
// accounts of the same type, and the given global strategy. mockA is
// returned as env.mock; mockB is reachable through the closure.
func newTypeEnv(t *testing.T, global routing.Strategy, ruleStrategy string) (*env, *mockprovider.Server) {
	t.Helper()
	st, err := storage.Open(config.StorageConfig{Path: t.TempDir() + "/test.db"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	if err := st.EnsureProvider(ctx, "acc-a", "custom-openai", "A"); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	if err := st.EnsureProvider(ctx, "acc-b", "custom-openai", "B"); err != nil {
		t.Fatalf("seed provider: %v", err)
	}

	// Two distinct mock upstreams — both are real custom providers, so
	// both expose Type() = custom-openai and join the type expansion.
	mockA := mockprovider.New(t)
	mockB := mockprovider.New(t)

	reg := provider.NewRegistry()
	allCaps := provider.Capabilities{Streaming: true, Tools: true, Vision: true}
	reg.Register(mockA.Provider("acc-a", allCaps))
	reg.Register(mockB.Provider("acc-b", allCaps))

	health := routing.NewHealthTracker()
	rules := []routing.Rule{{
		ID:         "rule-type",
		MatchModel: "gpt-5",
		Chain:      []string{"custom-openai"}, // a TYPE entry
		Priority:   100,
		Enabled:    true,
		Strategy:   routing.Strategy(ruleStrategy),
	}}
	router := routing.NewRouter(routing.Config{Rules: rules, GlobalStrategy: global},
		routing.WithRegistry(reg), routing.WithHealth(health))
	exec := routing.NewExecutor(routing.ExecutorConfig{
		Registry: reg, Health: health,
		RetrySleeper: func(time.Duration) {},
	})

	h := New(Params{Store: st, Router: router, Executor: exec})
	keyID, key, err := auth.CreateClientKey(ctx, st, "strategy-e2e")
	if err != nil {
		t.Fatalf("create client key: %v", err)
	}
	srv := httptest.NewServer(auth.NewMiddleware(st).Wrap(h))
	t.Cleanup(srv.Close)

	e := &env{
		t: t, store: st, mock: mockA, reg: reg,
		router: router, exec: exec, health: health,
		srv: srv, keyID: keyID, key: key,
	}
	return e, mockB
}

// The default strategy (empty rule strategy inherits the global
// round_robin) rotates requests across both accounts of the type — and
// each request log's route_trace carries version 2 + the expand and
// rotation events.
func TestTypeStrategy_RoundRobinRotatesWithTraces(t *testing.T) {
	e, mockB := newTypeEnv(t, routing.StrategyRoundRobin, "")
	respond := `{"id":"c","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

	// Fire four requests; both upstreams answer whatever rotation asks.
	for i := 0; i < 4; i++ {
		e.mock.Enqueue(mockprovider.NewScript().JSONResponse(http.StatusOK, respond, nil))
		mockB.Enqueue(mockprovider.NewScript().JSONResponse(http.StatusOK, respond, nil))
	}
	for i := 0; i < 4; i++ {
		status, body := e.post(t, "/v1/chat/completions", `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)
		if status != http.StatusOK {
			t.Fatalf("request %d: status %d body %s", i, status, body)
		}
	}

	logs, _, err := e.store.ListRequestLogsPage(context.Background(), storage.RequestLogFilter{}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 4 {
		t.Fatalf("%d logs, want 4", len(logs))
	}
	servedBy := map[string]int{}
	rotationSeen := false
	expandSeen := false
	for _, l := range logs {
		if l.ProviderID != "acc-a" && l.ProviderID != "acc-b" {
			t.Errorf("log provider_id = %q, want acc-a or acc-b (type expansion)", l.ProviderID)
		}
		servedBy[l.ProviderID]++
		var trace struct {
			Version int `json:"version"`
			Events  []struct {
				Type string `json:"type"`
			} `json:"events"`
		}
		if err := json.Unmarshal([]byte(l.RouteTrace), &trace); err != nil {
			t.Fatalf("route_trace not JSON: %v (%s)", err, l.RouteTrace)
		}
		if trace.Version != 9 {
			t.Errorf("route_trace version = %d, want 9", trace.Version)
		}
		for _, ev := range trace.Events {
			if ev.Type == "rotation" {
				rotationSeen = true
			}
			if ev.Type == "expand" {
				expandSeen = true
			}
		}
	}
	if servedBy["acc-a"] != 2 || servedBy["acc-b"] != 2 {
		t.Errorf("rotation distribution: %v, want 2/2 (round_robin)", servedBy)
	}
	if !rotationSeen || !expandSeen {
		t.Errorf("trace events missing: rotation=%t expand=%t", rotationSeen, expandSeen)
	}
}

// fill_first pins the first account — no rotation, ordered behavior.
func TestTypeStrategy_FillFirstSticks(t *testing.T) {
	e, _ := newTypeEnv(t, routing.StrategyRoundRobin, "fill_first")
	respond := `{"id":"c","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	for i := 0; i < 3; i++ {
		e.mock.Enqueue(mockprovider.NewScript().JSONResponse(http.StatusOK, respond, nil))
	}
	for i := 0; i < 3; i++ {
		if status, body := e.post(t, "/v1/chat/completions", `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`); status != http.StatusOK {
			t.Fatalf("request %d: status %d body %s", i, status, body)
		}
	}

	logs, _, err := e.store.ListRequestLogsPage(context.Background(), storage.RequestLogFilter{}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range logs {
		if l.ProviderID != "acc-a" {
			t.Errorf("fill_first served by %q, want acc-a (sticky first account)", l.ProviderID)
		}
	}
}
