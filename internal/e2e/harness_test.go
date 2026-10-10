package e2e
// End-to-end acceptance harness (Phase 1 DoD, Task #19): the OFFICIAL
// client SDKs — openai-go v3 and anthropic-sdk-go — drive a fully
// assembled proxy (auth middleware, httpapi, router, executor) over real
// HTTP against a mock upstream. This is the acceptance layer above the
// httpapi tests: it proves the SDK client → proxy → mock-upstream round
// trip works with zero hand-rolled HTTP, the way real clients run against
// PROxyLLM in production (plan §5.1, Analysis F E2E doctrine).
//
// SDK facts (live-verified against the pinned module sources,
// 2026-09-12):
//   - openai-go base URL carries "/v1" (option.WithBaseURL(srv+"/v1"));
//     retries default to 2 — pinned to 0 for deterministic mock errors.
//   - anthropic-sdk-go base URL is the bare server root; the method path
//     carries /v1/messages. Retries default to 2 — pinned to 0.
//
// Derived from plan §5.1/§5.3 + §6.4 (compat policy, route_trace) and the
// golden fixtures under testdata/golden/ — never from implementation
// behavior.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	openaiclient "github.com/openai/openai-go/v3"
	openaioption "github.com/openai/openai-go/v3/option"

	"github.com/tubeof/proxyllm/internal/auth"
	"github.com/tubeof/proxyllm/internal/config"
	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/server/httpapi"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/testutil/mockprovider"
	ttestutil "github.com/tubeof/proxyllm/internal/translate/testutil"
)

// testMasterKey is a fixed master key for the keyed-MAC client-key hasher in
// tests: production generates or reads one, and these tests only need the
// production WIRING, not production key material.
const testMasterKey = "master-key-material-for-tests"

// env is one assembled proxy-under-test plus the two official SDK clients
// aimed at it. mock-main (OpenAI chat wire, all capabilities) is the sole
// upstream.
type env struct {
	store *storage.Store
	mock  *mockprovider.Server
	srv   *httptest.Server
	// h is the underlying httpapi handler (auth middleware already wrapped
	// into srv); it lets benchmark variants re-wrap the same stack with a
	// different middleware configuration without reassembling everything.
	h       http.Handler
	openai  openaiclient.Client
	anthrop anthropic.Client
}

// newEnv assembles the default E2E environment: rules "gpt-5" and
// "claude-sonnet-5" → mock-main, one client key, both SDK clients pointed
// at the proxy with retries pinned to 0.
func newEnv(t *testing.T, rules ...routing.Rule) *env {
	t.Helper()
	if len(rules) == 0 {
		rules = []routing.Rule{
			{MatchModel: "gpt-5", Chain: []string{"mock-main"}, Priority: 100, Enabled: true},
			{MatchModel: "claude-sonnet-5", Chain: []string{"mock-main"}, Priority: 100, Enabled: true},
		}
	}

	st, err := storage.Open(config.StorageConfig{Path: t.TempDir() + "/test.db"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	if err := st.EnsureProvider(ctx, "mock-main", "custom", "Mock Main"); err != nil {
		t.Fatalf("seed provider row: %v", err)
	}

	mock := mockprovider.New(t)
	reg := provider.NewRegistry()
	reg.Register(mock.Provider("mock-main", provider.Capabilities{
		Streaming: true, Tools: true, Vision: true,
		WebSearch: true, WebFetch: true, CodeExecution: true, Thinking: true,
	}))

	// One shared HealthTracker gates the router and receives executor
	// reports (review M1 discipline).
	health := routing.NewHealthTracker()
	router := routing.NewRouter(routing.Config{Rules: rules},
		routing.WithRegistry(reg), routing.WithHealth(health))
	exec := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     reg,
		Health:       health,
		RetrySleeper: func(time.Duration) {},
	})

	h := httpapi.New(httpapi.Params{Store: st, Router: router, Executor: exec})

	// Production parity (2026-10-10): main derives the client-key hasher from
	// the master key, so a store without one would write legacy Argon2id rows and
	// every authenticated request in this suite would pay the KDF — a cost no
	// production request pays, and one that made the latency budgets measure the
	// wrong thing entirely.
	st.SetKeyHasher(storage.NewKeyHasher([]byte(testMasterKey)))
	_, key, err := auth.CreateClientKey(ctx, st, "e2e-test")
	if err != nil {
		t.Fatalf("create client key: %v", err)
	}

	// Neutralize ambient Claude Code credentials: this process runs inside a
	// Claude Code environment, so ANTHROPIC_AUTH_TOKEN/ANTHROPIC_BASE_URL are
	// typically set. The SDK folds the env token into a default
	// `Authorization: Bearer ...` header that survives our WithAPIKey option
	// (which only adds X-Api-Key), and the proxy's documented header
	// precedence (Bearer wins, §5.1 C5) would then authenticate the AMBIENT
	// token instead of the test key. Empty string suppresses the env default
	// (LookupEnv + `v != ""` guard in the SDK's DefaultClientOptions).
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")

	// OpenAI SDK env note: OPENAI_API_KEY/OPENAI_BASE_URL are deliberately NOT
	// neutralized. Unlike the anthropic SDK (see above), openai-go's
	// option.WithAPIKey/WithBaseURL use last-wins FIELD semantics — they
	// overwrite the APIKey/BaseURL fields themselves, so caller options always
	// supersede any ambient env default. Verified against the pinned module
	// source (client.go applyOptions / DefaultClientOptions).

	srv := httptest.NewServer(auth.NewMiddleware(st).Wrap(h))
	t.Cleanup(srv.Close)

	e := &env{
		store: st, mock: mock, srv: srv, h: h,
		// OpenAI SDK: base URL carries /v1; retries pinned 0 (default 2
		// would re-drive mock scripts and break deterministic error tests).
		openai: openaiclient.NewClient(
			openaioption.WithBaseURL(srv.URL+"/v1"),
			openaioption.WithAPIKey(key),
			openaioption.WithMaxRetries(0),
		),
		// Anthropic SDK: bare server root — the method path carries
		// /v1/messages; retries pinned 0.
		anthrop: anthropic.NewClient(
			option.WithBaseURL(srv.URL),
			option.WithAPIKey(key),
			option.WithMaxRetries(0),
		),
	}
	return e
}

// seedModel stores one model row (compat config) for the FK provider.
func (e *env) seedModel(t *testing.T, id string, compat json.RawMessage) {
	t.Helper()
	if err := e.store.PutModel(context.Background(), storage.Model{
		ID:         id,
		ProviderID: "mock-main",
		Name:       id,
		Compat:     compat,
	}); err != nil {
		t.Fatalf("seed model %s: %v", id, err)
	}
}

// requestLogs reads every stored request_logs row (newest first).
func (e *env) requestLogs(t *testing.T) []storage.RequestLog {
	t.Helper()
	logs, err := e.store.ListRequestLogs(context.Background(), 50)
	if err != nil {
		t.Fatalf("ListRequestLogs: %v", err)
	}
	return logs
}

// golden loads one golden fixture (relative to testdata/golden/).
func golden(t *testing.T, rel string) []byte {
	t.Helper()
	return ttestutil.LoadGolden(t, rel)
}

// chatResponseBody is the golden non-streaming upstream chat answer
// (testdata/golden/openai/chat-response.json verbatim): chatcmpl-abc123,
// assistant text + one tool_call (get_weather), finish_reason tool_calls,
// usage 1250/77. Every round-trip test asserts against these canonical
// values — the harness never invents its own wire shapes.
func chatResponseBody(t *testing.T) string {
	t.Helper()
	return string(golden(t, "openai/chat-response.json"))
}

// chatStreamScript is the golden chat stream (relayed deltas + terminal
// usage frame), scripted as one SSE script.
func (e *env) chatStreamScript(t *testing.T) *mockprovider.Script {
	t.Helper()
	return mockSSE(t, string(golden(t, "openai/chat-stream.sse")))
}

// mockJSON builds one scripted JSON upstream response.
func mockJSON(t *testing.T, status int, body string) *mockprovider.Script {
	t.Helper()
	s := mockprovider.NewScript()
	return s.JSONResponse(status, body, nil)
}

// mockSSE builds one scripted SSE upstream response from raw frames.
func mockSSE(t *testing.T, raw string) *mockprovider.Script {
	t.Helper()
	s := mockprovider.NewScript()
	return s.SSEFrames(raw)
}

// traceCompatEvent scans a request_logs route_trace for one compat_policy
// event with the given setting/value pair (§6.4: the trace shape is
// {"events":[{"type":"compat_policy","from":<setting>,"to":<value>}]}).
func traceCompatEvent(t *testing.T, raw json.RawMessage, setting, value string) bool {
	t.Helper()
	var trace struct {
		Events []struct {
			Type string `json:"type"`
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &trace); err != nil {
		t.Fatalf("route_trace not JSON: %v (%s)", err, raw)
	}
	for _, ev := range trace.Events {
		if ev.Type == "compat_policy" && ev.From == setting && ev.To == value {
			return true
		}
	}
	return false
}

// httpNoRouteStatus proves the 4xx discipline with the SDK's own error
// type (a helper used by the no-route tests).
func httpNoRouteStatus(t *testing.T, err error) int {
	t.Helper()
	var apiErr *openaiclient.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("SDK returned %v, want an API error", err)
	}
	return apiErr.StatusCode
}

// httpStatusOfAnthropic extracts the HTTP status from an Anthropic SDK
// error.
func httpStatusOfAnthropic(t *testing.T, err error) int {
	t.Helper()
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("SDK returned %v, want an API error", err)
	}
	return apiErr.StatusCode
}
