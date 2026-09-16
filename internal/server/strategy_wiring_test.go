package server

// RED tests for the ADR-009 wiring contracts, derived from the ADR (not
// implementation behavior):
//
//   - A routing-strategy PUT through the admin API reaches the LIVE router:
//     the next client request rotates (round_robin) or pins (fill_first)
//     without restart — the same live-swap contract as rule writes.
//   - A provider-type settings PUT reaches live construction: a stored
//     account row with no own base_url constructs against the type
//     settings' base_url after the write.
//   - The OmniRoute core behavior: an account created through the type
//     surface routes live traffic with no rule edit and no restart, and a
//     second account of the same type joins the rotation the same way.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/testutil/mockprovider"
)

// strategyChat sends one chat request through the proxy surface and
// returns the content string the upstream answered.
func strategyChat(t *testing.T, ts *httptest.Server, key string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := ensureBodyClosed(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat: status %d, body %s", resp.StatusCode, body)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("chat body not JSON: %v (%s)", err, body)
	}
	if len(parsed.Choices) != 1 {
		t.Fatalf("chat body: %s", body)
	}
	return parsed.Choices[0].Message.Content
}

// 1. The strategy PUT reaches the live router: with a type entry and two
// accounts, the global strategy decides rotation vs. stickiness — and a
// strategy write flips it for the very next request.
func TestServer_StrategyWriteReloadsLiveRouter(t *testing.T) {
	ctx := context.Background()
	cfg := newConfig(t)
	t.Setenv("PROXYLLM_ALLOW_PRIVATE_PROVIDER_URLS", "1")

	st, err := storage.Open(cfg.Storage)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	envelope, err := storage.NewEnvelope(wiringMasterKey(t))
	if err != nil {
		t.Fatalf("new envelope: %v", err)
	}
	st.SetEnvelope(envelope)

	// Two accounts of one type; a type-entry rule. The default strategy
	// (round_robin) rotates between them per request.
	accA, accB := mockprovider.New(t), mockprovider.New(t)
	for _, acc := range []struct {
		id  string
		srv *mockprovider.Server
	}{{"acc-a", accA}, {"acc-b", accB}} {
		if err := st.PutProvider(ctx, storage.Provider{
			ID: acc.id, Type: "custom-openai", Name: "Account " + acc.id,
			Config: json.RawMessage(fmt.Sprintf(`{"base_url":%q}`, acc.srv.URL())),
		}); err != nil {
			t.Fatalf("put provider %s: %v", acc.id, err)
		}
	}
	if err := st.PutRoutingRule(ctx, storage.RoutingRule{
		ID: "r1", MatchModel: "gpt-5", Chain: []string{"custom-openai"}, Priority: 100, Enabled: true,
	}); err != nil {
		t.Fatalf("put rule: %v", err)
	}

	srv, err := New(&cfg, st, nil, WithMasterKey(wiringMasterKey(t)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	adminTS := httptest.NewServer(srv)
	defer adminTS.Close()
	login := bootstrapAdmin(t, adminTS)

	_, key, err := authCreateKey(ctx, st)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	chat := func() string {
		accA.Enqueue(mockprovider.NewScript().JSONResponse(http.StatusOK,
			`{"id":"c","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"from-a"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil))
		accB.Enqueue(mockprovider.NewScript().JSONResponse(http.StatusOK,
			`{"id":"c","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"from-b"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil))
		return strategyChat(t, adminTS, key)
	}

	// Default round_robin: consecutive requests alternate accounts.
	first, second := chat(), chat()
	if first == second {
		t.Fatalf("default round_robin must rotate: both served by %q", first)
	}

	// The strategy write: fill_first pins the chain order. This must reach
	// the LIVE router — the next requests all start at the same account.
	resp := adminCall(t, adminTS, http.MethodPut, "/api/admin/routing-strategy",
		`{"strategy":"fill_first"}`, login)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put strategy: status %d, body %s", resp.StatusCode, ensureBodyClosed(t, resp))
	}
	_ = ensureBodyClosed(t, resp)

	a, b := chat(), chat()
	if a != b {
		t.Fatalf("after fill_first write: requests served by %q then %q — want sticky", a, b)
	}
}

// 2. The type-settings write reaches live construction: an account with no
// own base_url starts routing through the type settings' base_url after
// the PUT, with no restart and no account rewrite.
func TestServer_TypeSettingsWriteReachesLiveConstruction(t *testing.T) {
	ctx := context.Background()
	cfg := newConfig(t)
	t.Setenv("PROXYLLM_ALLOW_PRIVATE_PROVIDER_URLS", "1")

	st, err := storage.Open(cfg.Storage)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	envelope, err := storage.NewEnvelope(wiringMasterKey(t))
	if err != nil {
		t.Fatalf("new envelope: %v", err)
	}
	st.SetEnvelope(envelope)

	upstream := mockprovider.New(t)
	// The account has NO own config: its base_url comes from the type
	// settings the test writes through the admin API.
	if err := st.PutProvider(ctx, storage.Provider{
		ID: "acc-settings", Type: "custom-openai", Name: "Settings-fed account",
		Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("put provider: %v", err)
	}
	if err := st.PutRoutingRule(ctx, storage.RoutingRule{
		ID: "r1", MatchModel: "gpt-5", Chain: []string{"acc-settings"}, Priority: 100, Enabled: true,
	}); err != nil {
		t.Fatalf("put rule: %v", err)
	}

	srv, err := New(&cfg, st, nil, WithMasterKey(wiringMasterKey(t)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	adminTS := httptest.NewServer(srv)
	defer adminTS.Close()
	login := bootstrapAdmin(t, adminTS)

	_, key, err := authCreateKey(ctx, st)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	// Before: no base_url anywhere → the account cannot construct (the
	// row is skipped as not-registered) → no route.
	req, _ := http.NewRequest(http.MethodPost, adminTS.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode == http.StatusOK {
		_ = ensureBodyClosed(t, resp)
		t.Fatal("account without base_url must not route before the settings write")
	}
	_ = ensureBodyClosed(t, resp)

	// The type-settings write supplies the base_url. Live construction
	// must pick it up — the account routes without a restart.
	resp2 := adminCall(t, adminTS, http.MethodPut, "/api/admin/provider-types/custom-openai/settings",
		fmt.Sprintf(`{"settings":{"base_url":%q}}`, upstream.URL()), login)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("put type settings: status %d, body %s", resp2.StatusCode, ensureBodyClosed(t, resp2))
	}
	_ = ensureBodyClosed(t, resp2)

	upstream.Enqueue(mockprovider.NewScript().JSONResponse(http.StatusOK,
		`{"id":"c","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"live"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil))
	if got := strategyChat(t, adminTS, key); got != "live" {
		t.Fatalf("after type-settings write: content %q, want live (construction did not reload)", got)
	}
}

// 3. The OmniRoute core behavior (ADR-009 decision 2): a NEW account of a
// type joins a type-entry rule's rotation with no rule edit — created
// through the admin API, routing on the very next request.
func TestServer_NewAccountJoinsTypeRotationLive(t *testing.T) {
	ctx := context.Background()
	cfg := newConfig(t)
	t.Setenv("PROXYLLM_ALLOW_PRIVATE_PROVIDER_URLS", "1")

	st, err := storage.Open(cfg.Storage)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	envelope, err := storage.NewEnvelope(wiringMasterKey(t))
	if err != nil {
		t.Fatalf("new envelope: %v", err)
	}
	st.SetEnvelope(envelope)

	accA := mockprovider.New(t)
	if err := st.PutProvider(ctx, storage.Provider{
		ID: "acc-a", Type: "custom-openai", Name: "First",
		Config: json.RawMessage(fmt.Sprintf(`{"base_url":%q}`, accA.URL())),
	}); err != nil {
		t.Fatalf("put provider: %v", err)
	}
	if err := st.PutRoutingRule(ctx, storage.RoutingRule{
		ID: "r1", MatchModel: "gpt-5", Chain: []string{"custom-openai"}, Priority: 100, Enabled: true,
	}); err != nil {
		t.Fatalf("put rule: %v", err)
	}

	srv, err := New(&cfg, st, nil, WithMasterKey(wiringMasterKey(t)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	adminTS := httptest.NewServer(srv)
	defer adminTS.Close()
	login := bootstrapAdmin(t, adminTS)

	_, key, err := authCreateKey(ctx, st)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	// Before: one account serves everything.
	accA.Enqueue(mockprovider.NewScript().JSONResponse(http.StatusOK,
		`{"id":"c","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"a1"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil))
	if got := strategyChat(t, adminTS, key); got != "a1" {
		t.Fatalf("first request: %q, want a1", got)
	}

	// Create a SECOND account through the type surface — no rule edit. It
	// must join the rotation live.
	accB := mockprovider.New(t)
	resp := adminCall(t, adminTS, http.MethodPost, "/api/admin/provider-types/custom-openai/accounts",
		fmt.Sprintf(`{"name":"Second","api_key":"sk-wiring-2","config":{"base_url":%q}}`, accB.URL()), login)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create account: status %d, body %s", resp.StatusCode, ensureBodyClosed(t, resp))
	}
	_ = ensureBodyClosed(t, resp)

	// Next requests: round_robin (the default) rotates through BOTH
	// accounts — the new one is in the rotation with zero rule edits.
	accA.Enqueue(mockprovider.NewScript().JSONResponse(http.StatusOK,
		`{"id":"c","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"a2"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil))
	accB.Enqueue(mockprovider.NewScript().JSONResponse(http.StatusOK,
		`{"id":"c","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"b1"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil))
	accA.Enqueue(mockprovider.NewScript().JSONResponse(http.StatusOK,
		`{"id":"c","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"a3"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil))
	accB.Enqueue(mockprovider.NewScript().JSONResponse(http.StatusOK,
		`{"id":"c","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"b2"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil))

	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		seen[strategyChat(t, adminTS, key)] = true
	}
	if !seen["a2"] || !seen["a3"] || !seen["b1"] || !seen["b2"] {
		t.Fatalf("rotation did not cover both accounts: %v", seen)
	}
}
