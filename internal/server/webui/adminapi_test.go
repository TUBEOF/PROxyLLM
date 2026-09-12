package webui

// RED tests for the admin API (plan §5.2), derived exclusively from the
// committed storage v2 + session-auth contracts (Phase 2 research §10,
// S-/C- facts) and the Analysis G security doctrine — never from
// implementation behavior. The API is the WebUI SPA's only backend:
// admin bootstrap, providers CRUD, models incl. per-model compat, routing
// rules + reload, client keys (display-once), usage, request logs,
// settings KV.
//
// Contract pinned by these tests:
//   - POST   /api/admin/bootstrap            {"username","password"} (only while CountAdmins==0)
//   - POST   /api/admin/login                 (auth.SessionManager.HandleLogin)
//   - POST   /api/admin/logout                (auth.SessionManager.HandleLogout)
//   - GET    /api/admin/providers
//   - PUT    /api/admin/providers/{id}       {"type","name","config"}
//   - DELETE /api/admin/providers/{id}
//   - GET    /api/admin/models
//   - PUT    /api/admin/models/{id}           {"provider_id","name","capabilities","compat","priority"}
//   - DELETE /api/admin/models/{id}
//   - GET    /api/admin/routing
//   - PUT    /api/admin/routing/{id}          {"match_model","chain","priority","enabled"}
//   - DELETE /api/admin/routing/{id}
//   - POST   /api/admin/keys                  {"name"} → {"id","key"} (display-once)
//   - GET    /api/admin/keys
//   - POST   /api/admin/keys/{id}/revoke
//   - GET    /api/admin/usage?from=&to=       (inclusive YYYY-MM-DD day range)
//   - GET    /api/admin/logs?limit=&offset=&model=&provider_id=&status=&since=&until=
//   - GET    /api/admin/settings
//   - PUT    /api/admin/settings              {"key","value"}
//   - DELETE /api/admin/settings/{key}

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/proxyllm/proxyllm/internal/config"
	"github.com/proxyllm/proxyllm/internal/storage"
)

// ---------------------------------------------------------------------------
// Test environment
// ---------------------------------------------------------------------------

const testPassword = "correct horse battery staple"

// adminEnv is one admin API over a fresh store with a pinned clock.
type adminEnv struct {
	api  *AdminAPI
	st   *storage.Store
	now  int64
	keys reloadRecorder // records reload-hook invocations
}

// reloadRecorder captures routing-rule reload calls so tests can assert that
// WebUI edits reach the live router without restart.
type reloadRecorder struct {
	calls int
}

func (r *reloadRecorder) record() { r.calls++ }

// newAdminEnv builds the API under test over a fresh store.
func newAdminEnv(t *testing.T) *adminEnv {
	t.Helper()
	st, err := storage.Open(config.StorageConfig{Path: t.TempDir() + "/test.db"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	env := &adminEnv{st: st, now: 1_000_000}
	env.api = NewAdminAPI(AdminParams{
		Store:         st,
		ReloadRouting: env.keys.record,
		Now:           func() int64 { return env.now },
		MasterKey:     []byte("0123456789abcdef0123456789abcdef"),
	})
	return env
}

// bootstrap creates the first admin via the API (CountAdmins == 0 path) and
// returns the logged-in state.
func (e *adminEnv) bootstrap(t *testing.T, username string) loginState {
	t.Helper()
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, testPassword)
	w := e.call(t, http.MethodPost, "/api/admin/bootstrap", body, "")
	assertStatus(t, w, http.StatusOK, "bootstrap")

	return e.login(t, username, testPassword)
}

// loginState is what the SPA keeps after login: session cookie, csrf cookie,
// csrf token.
type loginState struct {
	sessionCookie string // full "name=value" pair of the session cookie
	csrfCookie    string // full "name=value" pair of the csrf cookie
	csrfToken     string
}

// cookieHeader rebuilds the Cookie header the SPA sends.
func (s loginState) cookieHeader() string {
	return s.sessionCookie + "; " + s.csrfCookie
}

// login issues a login request through the real mux, capturing the SPA state.
func (e *adminEnv) login(t *testing.T, username, password string) loginState {
	t.Helper()
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	w := e.call(t, http.MethodPost, "/api/admin/login", body, "")
	assertStatus(t, w, http.StatusOK, "login "+username)
	return parseLogin(t, w)
}

// call sends one request through the mux WITHOUT CSRF material (callers pass
// the cookie header themselves) and returns the recorder.
func (e *adminEnv) call(t *testing.T, method, path, body, cookieHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if cookieHeader != "" {
		req.Header.Set("Cookie", cookieHeader)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	e.api.Handler().ServeHTTP(w, req)
	return w
}

// authedCall is call with a logged-in admin's session + CSRF material:
// session cookies, application/json on bodies, X-CSRF-Token on mutations.
func (e *adminEnv) authedCall(t *testing.T, method, path, body string, s loginState) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Cookie", s.cookieHeader())
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if isMutation(method) {
		// C-4: the auth layer requires application/json on every mutation —
		// bodyless DELETEs included — so the SPA helper sets it always.
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", s.csrfToken)
	}
	w := httptest.NewRecorder()
	e.api.Handler().ServeHTTP(w, req)
	return w
}

// isMutation mirrors the auth layer's method gate for the test helper.
func isMutation(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// assertStatus fails with status + body when got != want.
func assertStatus(t *testing.T, w *httptest.ResponseRecorder, want int, what string) {
	t.Helper()
	if w.Code != want {
		t.Errorf("%s: status %d, want %d (body %s)", what, w.Code, want, w.Body.String())
	}
}

// decodeJSON unmarshals a response body or fails with the body for context.
func decodeJSON(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("response not JSON: %v (%s)", err, w.Body.String())
	}
}

// parseLogin extracts session cookie, csrf cookie, and csrf token from a
// login/bootstrap response (mirroring what the SPA must do).
func parseLogin(t *testing.T, w *httptest.ResponseRecorder) loginState {
	t.Helper()
	var s loginState
	for _, c := range w.Header().Values("Set-Cookie") {
		name, _, _ := strings.Cut(c, "=")
		value, _, _ := strings.Cut(c, ";")
		switch strings.TrimPrefix(name, "__Host-") {
		case "id":
			s.sessionCookie = value
		case "csrf":
			s.csrfCookie = value
		}
	}
	if w.Code == http.StatusOK {
		var payload struct {
			CSRFToken string `json:"csrf_token"`
		}
		decodeJSON(t, w, &payload)
		s.csrfToken = payload.CSRFToken
	}
	return s
}

// ---------------------------------------------------------------------------
// Scenarios
// ---------------------------------------------------------------------------

// §5.2: bootstrap creates the FIRST admin only; a second bootstrap must
// never be able to add or hijack admins.
func TestBootstrap_FirstAdminOnly(t *testing.T) {
	env := newAdminEnv(t)

	s := env.bootstrap(t, "operator")
	if s.sessionCookie == "" || s.csrfToken == "" {
		t.Fatalf("bootstrap did not yield a login: %+v", s)
	}

	// A second bootstrap — different operator, different password — is
	// rejected: the endpoint exists only for the zero-admin state.
	w := env.call(t, http.MethodPost, "/api/admin/bootstrap",
		`{"username":"attacker","password":"evil-pass-123"}`, "")
	assertStatus(t, w, http.StatusConflict, "second bootstrap")

	// The attacker cannot log in either.
	w = env.call(t, http.MethodPost, "/api/admin/login",
		`{"username":"attacker","password":"evil-pass-123"}`, "")
	assertStatus(t, w, http.StatusUnauthorized, "attacker login")
}

// Analysis G: every admin resource sits behind session auth; 401s are the
// uniform error shape, never distinguishing the failure layer.
func TestAdminEndpoints_RequireSession(t *testing.T) {
	env := newAdminEnv(t)
	env.bootstrap(t, "operator")

	for _, path := range []string{
		"/api/admin/providers", "/api/admin/models", "/api/admin/routing",
		"/api/admin/keys", "/api/admin/usage", "/api/admin/logs",
		"/api/admin/settings",
	} {
		w := env.call(t, http.MethodGet, path, "", "")
		assertStatus(t, w, http.StatusUnauthorized, "GET "+path)
		if !strings.Contains(w.Body.String(), `"error"`) {
			t.Errorf("%s: 401 body is not the uniform error shape: %s", path, w.Body.String())
		}
	}
}

// §5.2 providers CRUD: upsert + list + delete round-trip. Storage's
// secret-shaped-config rejection (Analysis G control 2: plain config must
// never hold credentials) must surface as 400.
func TestProviders_CRUDAndSecretConfigRejected(t *testing.T) {
	env := newAdminEnv(t)
	s := env.bootstrap(t, "operator")

	w := env.authedCall(t, http.MethodPut, "/api/admin/providers/prov-a",
		`{"type":"custom","name":"My endpoint","config":{"base_url":"https://api.example.com/v1"}}`, s)
	assertStatus(t, w, http.StatusOK, "put provider")

	// Secret-shaped config keys are refused at ANY depth — the API must
	// surface the storage guard as 400, never persist the value.
	w = env.authedCall(t, http.MethodPut, "/api/admin/providers/prov-b",
		`{"type":"custom","name":"Bad","config":{"nested":{"api_key":"sk-live-secret"}}}`, s)
	assertStatus(t, w, http.StatusBadRequest, "secret-shaped config")

	w = env.authedCall(t, http.MethodGet, "/api/admin/providers", "", s)
	assertStatus(t, w, http.StatusOK, "list providers")
	var list struct {
		Providers []struct {
			ID     string          `json:"id"`
			Type   string          `json:"type"`
			Name   string          `json:"name"`
			Config json.RawMessage `json:"config"`
		} `json:"providers"`
	}
	decodeJSON(t, w, &list)
	if len(list.Providers) != 1 || list.Providers[0].ID != "prov-a" {
		t.Fatalf("list: %+v", list.Providers)
	}
	if list.Providers[0].Type != "custom" || list.Providers[0].Name != "My endpoint" {
		t.Errorf("provider fields not round-tripped: %+v", list.Providers[0])
	}
	if !strings.Contains(string(list.Providers[0].Config), "api.example.com") {
		t.Errorf("config not round-tripped: %s", list.Providers[0].Config)
	}

	// Delete removes it; the list is honestly empty afterwards.
	w = env.authedCall(t, http.MethodDelete, "/api/admin/providers/prov-a", "", s)
	assertStatus(t, w, http.StatusOK, "delete provider")
	list.Providers = nil
	w = env.authedCall(t, http.MethodGet, "/api/admin/providers", "", s)
	decodeJSON(t, w, &list)
	if len(list.Providers) != 0 {
		t.Errorf("provider survived delete: %+v", list.Providers)
	}

	// Deleting a missing provider is 404 — never 500, never silent.
	w = env.authedCall(t, http.MethodDelete, "/api/admin/providers/ghost", "", s)
	assertStatus(t, w, http.StatusNotFound, "delete missing provider")
}

// §5.2 + §6.4 (OP-2): models carry per-model compatibility settings (compat
// JSON) — the "Models & Compatibility" editor's persistence backend. The
// provider FK guarantee must surface as 400.
func TestModels_CRUDAndCompatRoundTrip(t *testing.T) {
	env := newAdminEnv(t)
	s := env.bootstrap(t, "operator")
	w := env.authedCall(t, http.MethodPut, "/api/admin/providers/prov-a",
		`{"type":"custom","name":"My endpoint","config":{}}`, s)
	assertStatus(t, w, http.StatusOK, "put provider")

	w = env.authedCall(t, http.MethodPut, "/api/admin/models/gpt-5",
		`{"provider_id":"prov-a","name":"gpt-5","capabilities":{"vision":true},`+
			`"compat":{"thinking_handling":"strip","max_tokens_clamp":32000},"priority":5}`, s)
	assertStatus(t, w, http.StatusOK, "put model")

	// FK guarantee: a model cannot reference a provider that does not exist.
	w = env.authedCall(t, http.MethodPut, "/api/admin/models/ghost-model",
		`{"provider_id":"nope","name":"ghost"}`, s)
	assertStatus(t, w, http.StatusBadRequest, "model with unknown provider")

	w = env.authedCall(t, http.MethodGet, "/api/admin/models", "", s)
	assertStatus(t, w, http.StatusOK, "list models")
	var list struct {
		Models []struct {
			ID           string          `json:"id"`
			ProviderID   string          `json:"provider_id"`
			Name         string          `json:"name"`
			Capabilities json.RawMessage `json:"capabilities"`
			Compat       json.RawMessage `json:"compat"`
			Priority     int             `json:"priority"`
		} `json:"models"`
	}
	decodeJSON(t, w, &list)
	if len(list.Models) != 1 || list.Models[0].ID != "gpt-5" {
		t.Fatalf("list: %+v", list.Models)
	}
	if list.Models[0].ProviderID != "prov-a" || list.Models[0].Priority != 5 {
		t.Errorf("model fields not round-tripped: %+v", list.Models[0])
	}
	if !strings.Contains(string(list.Models[0].Compat), `"thinking_handling":"strip"`) {
		t.Errorf("per-model compat not round-tripped: %s", list.Models[0].Compat)
	}

	w = env.authedCall(t, http.MethodDelete, "/api/admin/models/gpt-5", "", s)
	assertStatus(t, w, http.StatusOK, "delete model")
	w = env.authedCall(t, http.MethodDelete, "/api/admin/models/gpt-5", "", s)
	assertStatus(t, w, http.StatusNotFound, "delete missing model")
}

// §5.2 + Analysis E: WebUI rule edits must reach the live router without a
// restart — every successful routing-rule write triggers the reload hook; a
// rejected write must not. Rules list priority-DESC.
func TestRoutingRules_CRUDTriggersReload(t *testing.T) {
	env := newAdminEnv(t)
	s := env.bootstrap(t, "operator")

	before := env.keys.calls
	w := env.authedCall(t, http.MethodPut, "/api/admin/routing/rule-low",
		`{"match_model":"claude-*","chain":["prov-a/gpt-5"],"priority":1,"enabled":true}`, s)
	assertStatus(t, w, http.StatusOK, "put rule-low")
	w = env.authedCall(t, http.MethodPut, "/api/admin/routing/rule-high",
		`{"match_model":"claude-opus-*","chain":["prov-a/o3"],"priority":9,"enabled":true}`, s)
	assertStatus(t, w, http.StatusOK, "put rule-high")
	if env.keys.calls != before+2 {
		t.Errorf("reload hook called %d times after 2 writes, want 2", env.keys.calls-before)
	}

	// A rejected write (empty chain) must NOT reload.
	w = env.authedCall(t, http.MethodPut, "/api/admin/routing/rule-bad",
		`{"match_model":"x","chain":[],"priority":1,"enabled":true}`, s)
	assertStatus(t, w, http.StatusBadRequest, "empty chain")
	if env.keys.calls != before+2 {
		t.Errorf("reload hook fired for a rejected write: %d calls", env.keys.calls-before)
	}

	w = env.authedCall(t, http.MethodGet, "/api/admin/routing", "", s)
	assertStatus(t, w, http.StatusOK, "list rules")
	var list struct {
		Rules []struct {
			ID         string   `json:"id"`
			MatchModel string   `json:"match_model"`
			Chain      []string `json:"chain"`
			Priority   int      `json:"priority"`
			Enabled    bool     `json:"enabled"`
		} `json:"rules"`
	}
	decodeJSON(t, w, &list)
	if len(list.Rules) != 2 {
		t.Fatalf("rules: %+v", list.Rules)
	}
	if list.Rules[0].ID != "rule-high" || list.Rules[1].ID != "rule-low" {
		t.Errorf("rules not priority-DESC: %+v", list.Rules)
	}
	if len(list.Rules[0].Chain) != 1 || list.Rules[0].Chain[0] != "prov-a/o3" {
		t.Errorf("chain not round-tripped: %+v", list.Rules[0].Chain)
	}

	w = env.authedCall(t, http.MethodDelete, "/api/admin/routing/rule-high", "", s)
	assertStatus(t, w, http.StatusOK, "delete rule")
	if env.keys.calls != before+3 {
		t.Errorf("delete did not reload the router: %d calls", env.keys.calls-before)
	}
}

// §5.2 + Analysis G control 1: client key secrets are display-once — the
// create response shows the full key exactly one time; no later response
// (list, revoke) ever contains the secret or the Argon2id hash.
func TestClientKeys_DisplayOnceAndHashFreeList(t *testing.T) {
	env := newAdminEnv(t)
	s := env.bootstrap(t, "operator")

	w := env.authedCall(t, http.MethodPost, "/api/admin/keys", `{"name":"claude-code laptop"}`, s)
	assertStatus(t, w, http.StatusOK, "create key")
	var created struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	decodeJSON(t, w, &created)
	if created.ID == "" || !strings.HasPrefix(created.Key, "pk_") {
		t.Fatalf("create response: %+v", created)
	}
	// The secret half (pk_<prefix>_<secret>) must never appear again.
	secret := created.Key[strings.LastIndex(created.Key, "_")+1:]

	w = env.authedCall(t, http.MethodGet, "/api/admin/keys", "", s)
	assertStatus(t, w, http.StatusOK, "list keys")
	if strings.Contains(w.Body.String(), secret) {
		t.Error("key list leaks the client key secret")
	}
	if strings.Contains(w.Body.String(), "$argon2") {
		t.Error("key list leaks the Argon2id hash")
	}
	var list struct {
		Keys []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			KeyPrefix string `json:"key_prefix"`
			Revoked   bool   `json:"revoked"`
		} `json:"keys"`
	}
	decodeJSON(t, w, &list)
	if len(list.Keys) != 1 || list.Keys[0].ID != created.ID {
		t.Fatalf("list: %+v", list.Keys)
	}
	if list.Keys[0].Name != "claude-code laptop" || list.Keys[0].KeyPrefix == "" {
		t.Errorf("key list fields: %+v", list.Keys[0])
	}
	if list.Keys[0].Revoked {
		t.Errorf("fresh key listed as revoked: %+v", list.Keys[0])
	}

	// Revoke flips the flag — revoked keys stay listed (audit trail).
	w = env.authedCall(t, http.MethodPost, "/api/admin/keys/"+created.ID+"/revoke", `{}`, s)
	assertStatus(t, w, http.StatusOK, "revoke key")
	list.Keys = nil
	w = env.authedCall(t, http.MethodGet, "/api/admin/keys", "", s)
	decodeJSON(t, w, &list)
	if len(list.Keys) != 1 || !list.Keys[0].Revoked {
		t.Errorf("revoked flag not visible after revoke: %+v", list.Keys)
	}
}

// §5.2 usage/stats: inclusive day-range query for the Overview dashboard; a
// matching-free range is honestly empty, never an error.
func TestUsage_InclusiveDayRange(t *testing.T) {
	env := newAdminEnv(t)
	s := env.bootstrap(t, "operator")

	ctx := context.Background()
	for _, u := range []storage.UsageDaily{
		{Day: "2026-09-10", ClientKeyID: "k1", ProviderID: "p1", Model: "m1",
			InputTokens: 10, OutputTokens: 5, Requests: 1},
		{Day: "2026-09-12", ClientKeyID: "k1", ProviderID: "p1", Model: "m1",
			InputTokens: 7, OutputTokens: 3, Requests: 2, Failures: 1},
	} {
		if err := env.st.IncrUsageDaily(ctx, u); err != nil {
			t.Fatal(err)
		}
	}

	var res struct {
		Rows []struct {
			Day         string `json:"day"`
			Model       string `json:"model"`
			InputTokens int64  `json:"input_tokens"`
			Requests    int64  `json:"requests"`
			Failures    int64  `json:"failures"`
		} `json:"rows"`
	}
	w := env.authedCall(t, http.MethodGet, "/api/admin/usage?from=2026-09-10&to=2026-09-12", "", s)
	assertStatus(t, w, http.StatusOK, "usage range")
	decodeJSON(t, w, &res)
	if len(res.Rows) != 2 {
		t.Fatalf("range rows: %+v", res.Rows)
	}
	if res.Rows[0].Day != "2026-09-10" || res.Rows[0].InputTokens != 10 {
		t.Errorf("row not round-tripped: %+v", res.Rows[0])
	}

	// Inclusive bounds: a from==to day holding a row returns it.
	res.Rows = nil
	w = env.authedCall(t, http.MethodGet, "/api/admin/usage?from=2026-09-12&to=2026-09-12", "", s)
	assertStatus(t, w, http.StatusOK, "usage single day")
	decodeJSON(t, w, &res)
	if len(res.Rows) != 1 || res.Rows[0].Day != "2026-09-12" {
		t.Errorf("inclusive bounds: %+v", res.Rows)
	}

	// A matching-free range is an empty 200 (honest empty state).
	res.Rows = nil
	w = env.authedCall(t, http.MethodGet, "/api/admin/usage?from=2026-08-01&to=2026-08-01", "", s)
	assertStatus(t, w, http.StatusOK, "usage empty range")
	decodeJSON(t, w, &res)
	if len(res.Rows) != 0 {
		t.Errorf("matching-free range must be empty: %+v", res.Rows)
	}
}

// §5.2 request logs: newest-first pages with filters for the log viewer;
// total lets the UI render page links without a second query per page.
func TestRequestLogs_PaginationAndFilters(t *testing.T) {
	env := newAdminEnv(t)
	s := env.bootstrap(t, "operator")
	ctx := context.Background()
	for i, m := range []string{"claude-opus-4", "gpt-5", "claude-opus-4"} {
		if _, err := env.st.InsertRequestLog(ctx, storage.RequestLog{
			Timestamp: int64(1000 + i), Model: m, ProviderID: "p1",
			Status: 200, LatencyMS: int64(10 + i),
			Usage:      json.RawMessage(`{"input_tokens":1}`),
			RouteTrace: json.RawMessage(`{"steps":[]}`),
		}); err != nil {
			t.Fatal(err)
		}
	}

	var res struct {
		Logs []struct {
			ID        int64           `json:"id"`
			Timestamp int64           `json:"timestamp"`
			Model     string          `json:"model"`
			Status    int             `json:"status"`
			Usage     json.RawMessage `json:"usage"`
		} `json:"logs"`
		Total int `json:"total"`
	}

	// Page one: limit 2 of total 3, newest first.
	w := env.authedCall(t, http.MethodGet, "/api/admin/logs?limit=2&offset=0", "", s)
	assertStatus(t, w, http.StatusOK, "logs page 1")
	decodeJSON(t, w, &res)
	if res.Total != 3 || len(res.Logs) != 2 {
		t.Fatalf("page: total=%d len=%d", res.Total, len(res.Logs))
	}
	if res.Logs[0].Timestamp <= res.Logs[1].Timestamp {
		t.Errorf("logs not newest-first: %+v", res.Logs)
	}
	if !strings.Contains(string(res.Logs[0].Usage), "input_tokens") {
		t.Errorf("usage JSON not round-tripped: %s", res.Logs[0].Usage)
	}

	// Model filter narrows both the page and the total.
	res.Logs, res.Total = nil, 0
	w = env.authedCall(t, http.MethodGet, "/api/admin/logs?model=claude-opus-4", "", s)
	assertStatus(t, w, http.StatusOK, "logs filtered")
	decodeJSON(t, w, &res)
	if res.Total != 2 || len(res.Logs) != 2 {
		t.Fatalf("filtered: total=%d len=%d", res.Total, len(res.Logs))
	}
	for _, l := range res.Logs {
		if l.Model != "claude-opus-4" {
			t.Errorf("filter leaked a non-matching row: %+v", l)
		}
	}
}

// §5.2 settings: config KV round-trip — the DB overrides layer (Analysis C)
// the Settings page edits.
func TestSettings_KVRoundTrip(t *testing.T) {
	env := newAdminEnv(t)
	s := env.bootstrap(t, "operator")

	w := env.authedCall(t, http.MethodPut, "/api/admin/settings",
		`{"key":"routing.non_claude","value":"warn"}`, s)
	assertStatus(t, w, http.StatusOK, "put setting")

	w = env.authedCall(t, http.MethodGet, "/api/admin/settings", "", s)
	assertStatus(t, w, http.StatusOK, "list settings")
	var list struct {
		Entries []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"entries"`
	}
	decodeJSON(t, w, &list)
	if len(list.Entries) != 1 || list.Entries[0].Key != "routing.non_claude" ||
		list.Entries[0].Value != "warn" {
		t.Fatalf("entries: %+v", list.Entries)
	}

	w = env.authedCall(t, http.MethodDelete, "/api/admin/settings/routing.non_claude", "", s)
	assertStatus(t, w, http.StatusOK, "delete setting")
	list.Entries = nil
	w = env.authedCall(t, http.MethodGet, "/api/admin/settings", "", s)
	decodeJSON(t, w, &list)
	if len(list.Entries) != 0 {
		t.Errorf("setting survived delete: %+v", list.Entries)
	}
}

// C-3/C-4 via the real mux: mutations without the CSRF header are 403;
// simple content types are 415 — a cross-site form must never be
// processable without a CORS preflight.
func TestAdminMutations_CSRFAndContentTypeEnforced(t *testing.T) {
	env := newAdminEnv(t)
	s := env.bootstrap(t, "operator")

	// Valid session, no X-CSRF-Token → 403.
	w := env.call(t, http.MethodPut, "/api/admin/providers/x",
		`{"type":"custom","name":"x","config":{}}`, s.cookieHeader())
	assertStatus(t, w, http.StatusForbidden, "mutation without csrf header")

	// Valid session + token, but text/plain → 415.
	req := httptest.NewRequest(http.MethodPut, "/api/admin/providers/x",
		strings.NewReader(`{"type":"custom"}`))
	req.Header.Set("Cookie", s.cookieHeader())
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-CSRF-Token", s.csrfToken)
	rec := httptest.NewRecorder()
	env.api.Handler().ServeHTTP(rec, req)
	assertStatus(t, rec, http.StatusUnsupportedMediaType, "text/plain mutation")
}

// S-10 via the mux: logout deletes the server-side session — the old cookie
// is dead afterwards.
func TestLogout_SessionKilled(t *testing.T) {
	env := newAdminEnv(t)
	s := env.bootstrap(t, "operator")

	w := env.authedCall(t, http.MethodPost, "/api/admin/logout", `{}`, s)
	assertStatus(t, w, http.StatusOK, "logout")

	w = env.call(t, http.MethodGet, "/api/admin/providers", "", s.cookieHeader())
	assertStatus(t, w, http.StatusUnauthorized, "old session after logout")
}
