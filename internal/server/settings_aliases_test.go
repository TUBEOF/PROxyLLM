package server

// Spec-derived settings → runtime tests (docs/configuration.md layer 4):
// the settings KV (WebUI-editable,
// SPA key `model_aliases`) is the DB override layer — values stored there
// must reach the live router at assembly AND on every successful settings
// write without restart. The alias map is a runtime swap (SetAliases), the
// same contract as rules (SetRules) and providers (ReplaceDynamics).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/testutil/mockprovider"
)

// 1. Assembly: an alias stored in the settings KV before server assembly
// routes a chat request sent under the alias name to the rule's chain.
func TestServer_ModelAliasesFromStoreRoute(t *testing.T) {
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
		ID: "r1", MatchModel: "primary-model", Chain: []string{"mock-main"}, Priority: 100, Enabled: true,
	}); err != nil {
		t.Fatalf("put rule: %v", err)
	}
	// The SPA writes a JSON object alias→model-id (Settings.tsx submitAliases).
	if err := st.SetConfigValue(ctx, "model_aliases", `{"alias-cc":"primary-model"}`); err != nil {
		t.Fatalf("set config: %v", err)
	}

	mock := mockprovider.New(t)
	srv, err := New(&cfg, st, nil,
		WithProvider(mock.Provider("mock-main", providerAllCaps())),
		WithMasterKey(wiringMasterKey(t)),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	_, key, err := authCreateKey(ctx, st)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	mock.Enqueue(mockprovider.NewScript().JSONResponse(http.StatusOK,
		`{"id":"cc-alias","object":"chat.completion","model":"primary-model","choices":[{"index":0,"message":{"role":"assistant","content":"via-alias"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil))

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"alias-cc","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := ensureBodyClosed(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat via alias: status %d, body %s", resp.StatusCode, body)
	}
	// The alias is a routing-side translation: the configured PRIMARY model
	// goes upstream; the client-facing response echoes the requested name
	// (pre-existing relay behavior). The upstream request is the spec-
	// relevant observable.
	sent := false
	for _, r := range mock.Requests() {
		if strings.Contains(string(r.Body), `"model":"primary-model"`) {
			sent = true
		}
	}
	if !sent {
		t.Errorf("upstream never saw the primary model — alias not resolved from settings KV")
	}
}

// 2. Live reload: a settings PUT through the admin API reaches the live
// router without restart — the alias is routable on the very next request
// (the KV was empty at server assembly), and deleting the setting
// un-resolves it again.
func TestServer_SettingsWriteReloadsLiveAliases(t *testing.T) {
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
		ID: "r1", MatchModel: "primary-model", Chain: []string{"mock-main"}, Priority: 100, Enabled: true,
	}); err != nil {
		t.Fatalf("put rule: %v", err)
	}

	mock := mockprovider.New(t)
	srv, err := New(&cfg, st, nil,
		WithProvider(mock.Provider("mock-main", providerAllCaps())),
		WithMasterKey(wiringMasterKey(t)),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	login := bootstrapAdmin(t, ts)
	_, key, err := authCreateKey(ctx, st)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	chat := func(model string) (int, string) {
		mock.Enqueue(mockprovider.NewScript().JSONResponse(http.StatusOK,
			`{"id":"cc","object":"chat.completion","model":"primary-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil))
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, ensureBodyClosed(t, resp)
	}

	// Before: no alias → unroutable.
	if st, _ := chat("alias-cc"); st != http.StatusNotFound {
		t.Fatalf("pre-alias chat: status %d, want 404", st)
	}

	// The WebUI save: settings PUT, then the NEXT chat resolves the alias.
	resp := adminCall(t, ts, http.MethodPut, "/api/admin/settings",
		`{"key":"model_aliases","value":"{\"alias-cc\":\"primary-model\"}"}`, login)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put setting: status %d, body %s", resp.StatusCode, ensureBodyClosed(t, resp))
	}
	defer func() { _ = resp.Body.Close() }()

	if st, _ := chat("alias-cc"); st != http.StatusOK {
		t.Fatalf("post-put chat: status %d, want 200 (aliases did not reload)", st)
	}

	// Deleting the setting un-resolves: the next request rejects again.
	dresp := adminCall(t, ts, http.MethodDelete, "/api/admin/settings/model_aliases", "", login)
	if dresp.StatusCode != http.StatusOK {
		t.Fatalf("delete setting: status %d, body %s", dresp.StatusCode, ensureBodyClosed(t, dresp))
	}
	defer func() { _ = dresp.Body.Close() }()
	if st, _ := chat("alias-cc"); st != http.StatusNotFound {
		t.Errorf("post-delete chat: status %d, want 404 (aliases not cleared)", st)
	}
}
