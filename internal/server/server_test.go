package server

// Server wiring RED tests (plan §4.1/§5.1): New must assemble the live
// proxy — routing rules loaded from the store, providers registered,
// Executor + Router injected into httpapi, auth middleware wrapping the
// mux (with the ENV opt-out per Analysis G control 1). Derived from the
// wiring contract, not implementation behavior.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/proxyllm/proxyllm/internal/auth"
	"github.com/proxyllm/proxyllm/internal/config"
	"github.com/proxyllm/proxyllm/internal/provider"
	"github.com/proxyllm/proxyllm/internal/storage"
	"github.com/proxyllm/proxyllm/internal/testutil/mockprovider"
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
