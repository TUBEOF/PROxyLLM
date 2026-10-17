package httpapi

// The alias path must keep the web-tool policy. The compatibility policy is
// resolved for the model the request LANDS on, so a client-typed Claude Code ID
// (claude-haiku-5-5 — no models row of its own, the whole point of a wildcard
// alias) picks up its alias target's row instead of the zero Compat.
//
// Before the fix the typed name was looked up, the lookup missed, the dated
// server-tool definition rode NATIVE, `requiredCapabilities` demanded
// web_fetch, the capability filter skipped the only account (a chat account's
// type declares no native web tool) and the request died 404 — no emulation, no
// web-tool fallback, no child row. Both paths were affected: the alias name and
// any model without a row.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/auth"
	"github.com/tubeof/proxyllm/internal/config"
	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/testutil/mockprovider"
)

// newAliasWebToolEnv assembles an Anthropic-surface environment whose chat
// account declares NO native web tool (the production shape: defaultCaps keeps
// web_search/web_fetch off) and whose model is reached through a wildcard alias
// over an implicit model row — exactly the routed path that used to 404.
func newAliasWebToolEnv(t *testing.T, rules ...routing.Rule) *env {
	t.Helper()
	st, err := storage.Open(config.StorageConfig{Path: t.TempDir() + "/test.db"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	if err := st.EnsureProvider(ctx, "acc-ollama", "ollama-cloud", "Ollama (test)"); err != nil {
		t.Fatalf("seed provider row: %v", err)
	}
	if err := st.PutModel(ctx, storage.Model{
		ID:         "ollama-cloud/m",
		ProviderID: "acc-ollama",
		Name:       "ollama-cloud/m",
		Compat:     json.RawMessage(`{"context_window":1048576}`),
	}); err != nil {
		t.Fatalf("seed model row: %v", err)
	}

	mock := mockprovider.New(t)
	reg := provider.NewRegistry()
	// Streaming and tools only: this account cannot serve web_search/web_fetch
	// natively, so an unconverted server-tool definition is a capability gap.
	reg.Register(mock.Provider("acc-ollama", provider.Capabilities{Streaming: true, Tools: true}))
	// A second account of the same shape: the chain fallover target.
	reg.Register(mock.Provider("acc-backup", provider.Capabilities{Streaming: true, Tools: true}))

	health := routing.NewHealthTracker()
	router := routing.NewRouter(routing.Config{
		Rules:   rules,
		Aliases: map[string]string{"claude-*": "ollama-cloud/m"},
	},
		routing.WithRegistry(reg), routing.WithHealth(health),
		routing.WithModelLookup(func(ctx context.Context, modelID string) (string, bool) {
			row, err := st.GetModel(ctx, modelID)
			if err != nil || row.ProviderID == "" {
				return "", false
			}
			return row.ProviderID, true
		}))
	exec := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     reg,
		Health:       health,
		RetrySleeper: func(time.Duration) {},
	})

	h := New(Params{Store: st, Router: router, Executor: exec})
	st.SetKeyHasher(storage.NewKeyHasher([]byte(testMasterKey)))
	keyID, key, err := auth.CreateClientKey(ctx, st, "alias-webtool-test")
	if err != nil {
		t.Fatalf("create client key: %v", err)
	}
	srv := httptest.NewServer(auth.NewMiddleware(st).Wrap(h))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { WaitForPendingCaptures(5 * time.Second) })

	return &env{t: t, store: st, mock: mock, reg: reg, router: router, exec: exec,
		health: health, srv: srv, keyID: keyID, key: key}
}

// webFetchRequest is the Anthropic wire shape a client sends when it offers the
// web_fetch server tool: a dated type string, no input_schema.
const webFetchRequest = `{
  "model": "claude-haiku-5-5",
  "max_tokens": 256,
  "messages": [{"role": "user", "content": "fetch https://example.com"}],
  "tools": [{"type": "web_fetch_20250910", "name": "web_fetch", "max_uses": 3}]
}`

// TestAliasedWebToolRequestRoutesAndEmulates is the regression: an aliased
// request carrying a web_fetch server-tool definition must route (200) and the
// definition must reach the upstream CONVERTED into the emulated client tool —
// never as the dated server-tool type, which is what made the request
// unroutable.
func TestAliasedWebToolRequestRoutesAndEmulates(t *testing.T) {
	e := newAliasWebToolEnv(t)
	e.mock.Enqueue(mockprovider.NewScript().JSONResponse(200, `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil))

	status, body := e.post(t, "/v1/messages", webFetchRequest)
	if status != 200 {
		t.Fatalf("status = %d, body %s (the aliased web-tool request must not be rejected)", status, body)
	}

	reqs := e.mock.Requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(reqs))
	}
	upstream := reqs[0].Body
	if strings.Contains(upstream, "web_fetch_2025") {
		t.Errorf("the dated server-tool type rode to the upstream: %s", upstream)
	}
	if !strings.Contains(upstream, `"web_fetch"`) || !strings.Contains(upstream, `"url"`) {
		t.Errorf("the emulated web_fetch client tool is missing from the upstream request: %s", upstream)
	}

	// The trace tells the operator what happened: the alias hop and the applied
	// emulation policy (never silent).
	logs, err := e.store.ListRequestLogs(context.Background(), 5)
	if err != nil {
		t.Fatalf("list request logs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("request log rows = %d, want 1", len(logs))
	}
	var trace struct {
		Events []struct {
			Type       string `json:"type"`
			From       string `json:"from"`
			To         string `json:"to"`
			Reason     string `json:"reason"`
			ProviderID string `json:"provider_id"`
		} `json:"events"`
	}
	if err := json.Unmarshal(logs[0].RouteTrace, &trace); err != nil {
		t.Fatalf("route_trace is not the documented {version,events} shape: %v (%s)", err, logs[0].RouteTrace)
	}
	var alias, emulate, attempt bool
	for _, ev := range trace.Events {
		switch {
		case ev.Type == "alias" && ev.From == "claude-haiku-5-5" && ev.To == "ollama-cloud/m":
			alias = true
		case ev.Type == "compat_policy" && ev.To == "emulate-firecrawl":
			emulate = true
		case ev.Type == "attempt" && ev.ProviderID == "acc-ollama":
			attempt = true
		}
	}
	if !alias || !emulate || !attempt {
		t.Errorf("route_trace is missing alias=%v emulate=%v attempt=%v: %s", alias, emulate, attempt, logs[0].RouteTrace)
	}
}

// The same request for the model's OWN name (a row-backed model, no alias hop)
// keeps working — the fix must not trade one path for the other.
func TestRowBackedWebToolRequestRoutesAndEmulates(t *testing.T) {
	e := newAliasWebToolEnv(t)
	e.mock.Enqueue(mockprovider.NewScript().JSONResponse(200, `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil))

	status, body := e.post(t, "/v1/messages", strings.Replace(webFetchRequest,
		`"model": "claude-haiku-5-5"`, `"model": "ollama-cloud/m"`, 1))
	if status != 200 {
		t.Fatalf("status = %d, body %s", status, body)
	}
	if got := e.mock.Requests(); len(got) != 1 || strings.Contains(got[0].Body, "web_fetch_2025") {
		t.Fatalf("upstream request = %+v, want the converted emulated tool", got)
	}
}

// An alias resolves ONTO the configured routing rule, so the rule's fallback
// chain is the alias request's chain: the first account answers 500, the second
// serves — and the trace names both (failovers are never silent).
func TestAliasedWebToolRequestHonoursTheConfiguredChain(t *testing.T) {
	e := newAliasWebToolEnv(t, routing.Rule{
		MatchModel: "ollama-cloud/m",
		Chain:      []string{"acc-ollama", "acc-backup"},
		Priority:   100,
		Enabled:    true,
	})
	e.mock.Enqueue(
		mockprovider.NewScript().Fail(500, "upstream exploded", ""),
		mockprovider.NewScript().JSONResponse(200, `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil),
	)

	status, body := e.post(t, "/v1/messages", webFetchRequest)
	if status != 200 {
		t.Fatalf("status = %d, body %s (the alias request must ride the rule's chain)", status, body)
	}
	reqs := e.mock.Requests()
	if len(reqs) != 2 {
		t.Fatalf("upstream attempts = %d, want 2 (failover to the chain's second entry)", len(reqs))
	}
	logs, err := e.store.ListRequestLogs(context.Background(), 5)
	if err != nil || len(logs) != 1 {
		t.Fatalf("request log rows = %d (err %v), want 1", len(logs), err)
	}
	if !strings.Contains(string(logs[0].RouteTrace), `"failover"`) ||
		!strings.Contains(string(logs[0].RouteTrace), `"acc-backup"`) {
		t.Errorf("the chain fallover is not traced: %s", logs[0].RouteTrace)
	}
}

// The degrade contract with no web-tool account configured (no emulator wired in
// the executor): the request still ROUTES and the model's call is answered with
// the documented `unavailable` error result — the model can tell the client it
// could not look the request up. Before the fix this shape never got that far:
// the definitions rode native against an account without native web tools, and
// the whole request 404'd instead of degrading one tool call.
func TestWebToolWithoutBackendDegradesInsteadOfFailingTheRequest(t *testing.T) {
	e := newAliasWebToolEnv(t)
	// Round 1: the model calls the emulated tool. Round 2: it answers in words.
	e.mock.Enqueue(
		mockprovider.NewScript().JSONResponse(200, `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"web_fetch","arguments":"{\"url\":\"https://example.com\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil),
		mockprovider.NewScript().JSONResponse(200, `{"id":"y","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"I could not fetch it."},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil),
	)

	// The URL must be in the conversation: the emulation layer refuses to fetch
	// a URL the client never mentioned (the documented url_not_in_prior_context).
	req := `{
	  "model": "claude-haiku-5-5",
	  "max_tokens": 256,
	  "messages": [{"role": "user", "content": "fetch https://example.com"}],
	  "tools": [{"type": "web_fetch_20250910", "name": "web_fetch", "max_uses": 3}]
	}`
	status, body := e.post(t, "/v1/messages", req)
	if status != 200 {
		t.Fatalf("status = %d, body %s (the request must route and degrade, not 404)", status, body)
	}
	if !strings.Contains(body, "unavailable") {
		t.Errorf("the tool call did not carry the documented unavailable error result: %s", body)
	}
	if got := e.mock.Requests(); len(got) != 2 {
		t.Errorf("upstream attempts = %d, want 2 (the loop answers the call and re-dispatches)", len(got))
	}
}
