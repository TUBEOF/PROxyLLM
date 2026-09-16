package server

// Server wiring RED tests (plan §4.1/§5.1): New must assemble the live
// proxy — routing rules loaded from the store, providers registered,
// Executor + Router injected into httpapi, auth middleware wrapping the
// mux (with the ENV opt-out per Analysis G control 1). Derived from the
// wiring contract, not implementation behavior.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/auth"
	"github.com/tubeof/proxyllm/internal/config"
	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/testutil/mockprovider"
	"github.com/tubeof/proxyllm/internal/tools/firecrawl"
)

// authCreateKey creates one client key directly in the store (test
// shortcut around the WebUI flow that will own this later).
func authCreateKey(ctx context.Context, st *storage.Store) (string, string, error) {
	return auth.CreateClientKey(ctx, st, "server-test")
}

// newConfig returns one minimal live config over a fresh temp-dir
// database.
func newConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{Storage: config.StorageConfig{Path: t.TempDir() + "/test.db"}}
}

// 1. Routing rules persist in the store and are loaded by New: a rule
// stored before server assembly routes a chat request through the
// registered mock provider to a scripted upstream response.
func TestServer_LoadsRoutingRulesFromStore(t *testing.T) {
	ctx := context.Background()
	cfg := newConfig(t)

	// Store the rule first — New must read it, not require in-memory
	// registration (Analysis E: rule state lives in the DB, WebUI-editable).
	st, err := storage.Open(cfg.Storage)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	if err := st.EnsureProvider(ctx, "mock-main", "custom", "Mock Main"); err != nil {
		t.Fatalf("ensure provider: %v", err)
	}
	if err := st.PutRoutingRule(ctx, storage.RoutingRule{
		ID:         "r1",
		MatchModel: "gpt-5",
		Chain:      []string{"mock-main"},
		Priority:   100,
		Enabled:    true,
	}); err != nil {
		t.Fatalf("put rule: %v", err)
	}

	mock := mockprovider.New(t)
	srv, err := New(&cfg, st, nil,
		WithProvider(mock.Provider("mock-main", providerAllCaps())),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mock.Enqueue(mockprovider.NewScript().JSONResponse(http.StatusOK,
		`{"id":"chatcmpl-wired","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"wired"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, nil))

	// Anonymous request path: auth default-on would need a key; the test
	// creates one via the store and sends it.
	_, key, err := authCreateKey(ctx, st)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (rules from store must route)", resp.StatusCode)
	}
	var body struct {
		ID     string `json:"id"`
		Object string `json:"object"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body.Object != "chat.completion" || body.ID != "chatcmpl-wired" {
		t.Errorf("body = %+v, want wired chat.completion relayed", body)
	}
}

// 2. Auth-disabled opt-out (Analysis G control 1): with the ENV-derived
// flag set, the same request succeeds without any client key.
func TestServer_AuthDisabledOptOut(t *testing.T) {
	ctx := context.Background()
	cfg := newConfig(t)
	cfg.Auth.Disabled = true

	st, err := storage.Open(cfg.Storage)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	if err := st.EnsureProvider(ctx, "mock-main", "custom", "Mock Main"); err != nil {
		t.Fatalf("ensure provider: %v", err)
	}
	if err := st.PutRoutingRule(ctx, storage.RoutingRule{
		ID:         "r1",
		MatchModel: "gpt-5",
		Chain:      []string{"mock-main"},
		Priority:   100,
		Enabled:    true,
	}); err != nil {
		t.Fatalf("put rule: %v", err)
	}

	mock := mockprovider.New(t)
	srv, err := New(&cfg, st, nil,
		WithProvider(mock.Provider("mock-main", providerAllCaps())),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mock.Enqueue(mockprovider.NewScript().JSONResponse(http.StatusOK,
		`{"id":"chatcmpl-anon","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"anon"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil))

	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 with auth disabled (ENV opt-out)", resp.StatusCode)
	}
}

// providerAllCaps is every capability — capability filtering must not
// drop the mock for the requests these tests send.
func providerAllCaps() provider.Capabilities {
	return provider.Capabilities{
		Streaming: true, Tools: true, Vision: true,
		WebSearch: true, WebFetch: true, CodeExecution: true, Thinking: true,
	}
}

// 3. WithEmulator (plan §6.2): a server assembled with an emulator must
// answer an emulated web_search end-to-end — the executor routes the
// model's tool call through the emulator and re-dispatches, so the client
// sees the synthesized server_tool_use + web_search_tool_result blocks
// (never silent).
func TestServer_WithEmulatorAnswersWebSearch(t *testing.T) {
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
		ID:         "r1",
		MatchModel: "gpt-5",
		Chain:      []string{"mock-main"},
		Priority:   100,
		Enabled:    true,
	}); err != nil {
		t.Fatalf("put rule: %v", err)
	}

	// The model row carries the emulate-firecrawl policy (§6.4): compat
	// rows in the store, not request flags, govern emulation.
	if err := st.PutModel(ctx, storage.Model{
		ID:         "gpt-5",
		ProviderID: "mock-main",
		Name:       "gpt-5",
		Compat:     json.RawMessage(`{"server_tool_emulation":"emulate-firecrawl"}`),
	}); err != nil {
		t.Fatalf("put model: %v", err)
	}

	// Firecrawl-shaped upstream behind the emulator.
	fc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"web":[{"title":"Go","url":"https://go.dev/"}]}}`))
	}))
	defer fc.Close()

	// Mock model: round 1 answers with the web_search tool call, round 2
	// answers with text.
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

	// Anthropic surface with a server-tool web_search def: the stored
	// emulate-firecrawl policy — not a request flag — turns this def into
	// an emulated tool upstream.
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
		t.Fatalf("status = %d, want 200 (emulated round trip)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var msg struct {
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	var sawUse, sawResult bool
	for _, blk := range msg.Content {
		switch blk.Type {
		case "server_tool_use":
			sawUse = true
		case "web_search_tool_result":
			sawResult = true
		}
	}
	if !sawUse || !sawResult {
		t.Errorf("content types = %+v, want server_tool_use + web_search_tool_result (emulation must be protocol-conform and visible)", msg.Content)
	}
}
