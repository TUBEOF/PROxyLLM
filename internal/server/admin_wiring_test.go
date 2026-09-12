package server

// RED tests for the admin-API wiring into the live server (plan §5.2, §9:
// the WebUI backend sits under /api/admin/ on the SAME mux as the proxy
// surfaces, and routing-rule writes must reach the LIVE router without
// restart — Analysis E: rule state is DB-owned and WebUI-editable).
//
// Contract pinned by these tests:
//   - WithMasterKey mounts the admin API: /api/admin/bootstrap answers
//     (200 on the zero-admin store), and the mounted surface enforces
//     session auth (401 without one).
//   - Without WithMasterKey no admin surface is mounted: /api/admin/*
//     falls through to the client-key-gated proxy surface ("/") and gets
//     its uniform 401. The admin logic is unreachable — the master key is
//     the session/CSRF HMAC secret, so no key means no admin API, never a
//     weakened fallback.
//   - A routing-rule PUT through the admin API re-routes LIVE traffic:
//     the next client request follows the NEW chain without restart, and
//     a rule DELETE restores the previous routing the same way.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/proxyllm/proxyllm/internal/storage"
	"github.com/proxyllm/proxyllm/internal/testutil/mockprovider"
)

// wiringMasterKey is a fresh 32-byte master key per test — never a shared
// constant, mirroring production where the key never leaves loadMasterKey's
// scope.
func wiringMasterKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return key
}

// wiringLogin captures what the SPA keeps after bootstrap: session cookie
// pair, csrf cookie pair, csrf token.
type wiringLogin struct {
	sessionCookie string // "name=value" pair
	csrfCookie    string
	csrfToken     string
}

// bootstrapAdmin creates the first admin over the mounted /api/admin/ and
// returns the logged-in state (mirroring what the SPA keeps: cookie pairs
// and the csrf token).
func bootstrapAdmin(t *testing.T, ts *httptest.Server) wiringLogin {
	t.Helper()
	resp, err := http.Post(ts.URL+"/api/admin/bootstrap", "application/json",
		strings.NewReader(`{"username":"operator","password":"wiring-pass-123"}`))
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read bootstrap body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bootstrap: status %d, body %s", resp.StatusCode, raw)
	}

	var login wiringLogin
	for _, c := range resp.Header.Values("Set-Cookie") {
		// Each Set-Cookie value: "name=value; attrs..." — the pair is the
		// part before the first semicolon.
		pair, _, _ := strings.Cut(c, ";")
		name, _, _ := strings.Cut(pair, "=")
		switch strings.TrimPrefix(name, "__Host-") {
		case "id":
			login.sessionCookie = pair
		case "csrf":
			login.csrfCookie = pair
		}
	}
	var body struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("bootstrap body not JSON: %v", err)
	}
	login.csrfToken = body.CSRFToken
	return login
}

// adminCall issues a session-authenticated admin call (C-3/C-4: cookies,
// X-CSRF-Token, and Content-Type on every mutation).
func adminCall(t *testing.T, ts *httptest.Server, method, path, body string, l wiringLogin) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", l.sessionCookie+"; "+l.csrfCookie)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", l.csrfToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// ensureBodyClosed drains and closes a response, returning its body bytes.
func ensureBodyClosed(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(raw)
}

// 1. WithMasterKey mounts the admin surface: bootstrap answers on the
// zero-admin store and the mounted surface enforces sessions (401 without
// one). Without the option, /api/admin/* falls through to the client-key
// gate (uniform 401) — the admin logic is unreachable, no keyless
// degraded mount.
func TestServer_AdminAPIMountedBehindMasterKey(t *testing.T) {
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

	// mockprovider.New registers its own t.Cleanup close.
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

	// Mounted: bootstrap succeeds on the zero-admin store.
	login := bootstrapAdmin(t, ts)
	if login.sessionCookie == "" || login.csrfToken == "" {
		t.Fatalf("bootstrap did not yield a login: %+v", login)
	}

	// Session-gated: an anonymous GET must 401 with the uniform body.
	resp, err := http.Get(ts.URL + "/api/admin/providers")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous providers GET: status %d, want 401", resp.StatusCode)
	}
	if !strings.Contains(ensureBodyClosed(t, resp), `"error"`) {
		t.Errorf("401 body is not the uniform error shape")
	}
}

// 2. No WithMasterKey → no admin surface at all: the request falls through
// to the client-key gate, never a keyless degraded admin mount.
func TestServer_NoMasterKeyNoAdminAPI(t *testing.T) {
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
	srv, err := New(&cfg, st, nil,
		WithProvider(mock.Provider("mock-main", providerAllCaps())),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()

	// No admin surface is mounted, so the request falls through to the
	// client-key-gated proxy surface ("/") — the uniform 401. The admin
	// logic is never reachable: no key, no admin API, never a weakened
	// fallback.
	resp, err := http.Post(ts.URL+"/api/admin/bootstrap", "application/json",
		strings.NewReader(`{"username":"x","password":"yyyy-zzzz-1234"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bootstrap without master key: status %d, want 401 (fall-through to the client-key gate; admin surface must not mount)", resp.StatusCode)
	}
	if !strings.Contains(ensureBodyClosed(t, resp), `"error"`) {
		t.Errorf("401 body is not the uniform error shape")
	}
}

// 3. Live reload end-to-end: a routing-rule PUT through the admin API
// re-routes the very next client request — the live router picks up the
// new chain without restart — and the rule DELETE restores routing the
// same way.
func TestServer_RoutingWriteReloadsLiveRouter(t *testing.T) {
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
	if err := st.EnsureProvider(ctx, "mock-backup", "custom", "Mock Backup"); err != nil {
		t.Fatalf("ensure provider: %v", err)
	}
	if err := st.PutRoutingRule(ctx, storage.RoutingRule{
		ID: "r1", MatchModel: "gpt-5", Chain: []string{"mock-main"}, Priority: 100, Enabled: true,
	}); err != nil {
		t.Fatalf("put rule: %v", err)
	}

	main := mockprovider.New(t)
	backup := mockprovider.New(t)
	srv, err := New(&cfg, st, nil,
		WithProvider(main.Provider("mock-main", providerAllCaps())),
		WithProvider(backup.Provider("mock-backup", providerAllCaps())),
		WithMasterKey(wiringMasterKey(t)),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	adminTS := httptest.NewServer(srv)
	defer adminTS.Close()
	login := bootstrapAdmin(t, adminTS)

	// Client key for the proxy surface.
	_, key, err := authCreateKey(ctx, st)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	chat := func(ts *httptest.Server) string {
		main.Enqueue(mockprovider.NewScript().JSONResponse(http.StatusOK,
			`{"id":"c","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"from-main"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil))
		backup.Enqueue(mockprovider.NewScript().JSONResponse(http.StatusOK,
			`{"id":"c","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"from-backup"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil))
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
			t.Fatalf("chat body not JSON: %v", err)
		}
		if len(parsed.Choices) != 1 {
			t.Fatalf("chat body: %s", body)
		}
		return parsed.Choices[0].Message.Content
	}

	// Before: rule r1 routes gpt-5 to mock-main.
	if got := chat(adminTS); got != "from-main" {
		t.Fatalf("before rule write: content %q, want from-main", got)
	}

	// The admin rule write: gpt-5 re-routes to mock-backup at higher
	// priority. This must reach the LIVE router.
	resp := adminCall(t, adminTS, http.MethodPut, "/api/admin/routing/r2",
		`{"match_model":"gpt-5","chain":["mock-backup"],"priority":200,"enabled":true}`, login)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put rule r2: status %d, body %s", resp.StatusCode, ensureBodyClosed(t, resp))
	}
	_ = ensureBodyClosed(t, resp)

	if got := chat(adminTS); got != "from-backup" {
		t.Fatalf("after rule write: content %q, want from-backup (live router did not reload)", got)
	}

	// DELETE restores the previous routing without restart.
	resp = adminCall(t, adminTS, http.MethodDelete, "/api/admin/routing/r2", "", login)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete rule r2: status %d, body %s", resp.StatusCode, ensureBodyClosed(t, resp))
	}
	_ = ensureBodyClosed(t, resp)

	if got := chat(adminTS); got != "from-main" {
		t.Fatalf("after rule delete: content %q, want from-main (live router did not reload)", got)
	}
}
