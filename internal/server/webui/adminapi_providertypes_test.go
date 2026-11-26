package webui

// RED tests for the ADR-009 admin API surface, derived from the ADR (not
// implementation behavior): the provider-type catalog, per-type settings +
// account management, and the routing-strategy endpoints. Every contract
// the WebUI's form-based pages depend on is pinned here:
//
//   - GET  /api/admin/provider-types            → catalog: type, label, description,
//     auth (api_key|oauth), settings_fields, account_fields, default_capabilities
//   - GET  /api/admin/provider-types/{type}     → {settings, accounts:[{id,type,name,has_credential}]}
//   - PUT  /api/admin/provider-types/{type}/settings  {"settings":{...}} (validated per type, SSRF guard)
//   - POST /api/admin/provider-types/{type}/accounts  {"name","api_key"?} → accountDTO (server-minted id)
//   - GET  /api/admin/routing-strategy          → {"strategy":"round_robin"|"fill_first"}
//   - PUT  /api/admin/routing-strategy          {"strategy"} (invalid → 400)
//   - PUT  /api/admin/routing/{id}              gains "strategy" (""|round_robin|fill_first)
//   - GET  /api/admin/routing                   rules carry strategy
//
// Security doctrine pinned: session+CSRF gating on every new route, no
// secret ever rides a response (accountDTO carries has_credential only).

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestProviderTypes_Catalog(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")

	w := e.authedCall(t, http.MethodGet, "/api/admin/provider-types", "", s)
	assertStatus(t, w, http.StatusOK, "catalog")
	var resp struct {
		Types []struct {
			Type              string `json:"type"`
			Label             string `json:"label"`
			Description       string `json:"description"`
			Auth              string `json:"auth"`
			OAuthFlow         string `json:"oauth_flow"`
			RiskNotice        string `json:"risk_notice"`
			RiskNoticeVersion int    `json:"risk_notice_version"`
			RiskAcknowledged  bool   `json:"risk_acknowledged"`
		} `json:"types"`
	}
	decodeJSON(t, w, &resp)
	// ADR-010: firecrawl joined the catalog as the first web_tool kind;
	// ollama-cloud-web is the second (ADR-015 — the same vendor's hosted
	// web-tool API, a distinct account type from its chat endpoint).
	// anthropic and ollama-native are native upstream protocols.
	// ADR-021 removed gemini, openai-responses and openai-decisions;
	// ADR-022 added anthropic-oauth.
	if len(resp.Types) != 8 {
		t.Fatalf("catalog has %d types, want 8 (custom-openai, anthropic, anthropic-oauth, ollama-cloud, ollama-native, codex-oauth, firecrawl, ollama-cloud-web)", len(resp.Types))
	}
	kinds := map[string]string{}
	for _, pt := range resp.Types {
		kinds[pt.Type] = pt.Auth
	}
	if kinds["ollama-cloud"] != "api_key" || kinds["codex-oauth"] != "oauth" || kinds["firecrawl"] != "api_key" {
		t.Errorf("auth kinds: %v (ollama-cloud=api_key, codex-oauth=oauth, firecrawl=api_key expected)", kinds)
	}
	if kinds["ollama-cloud-web"] != "api_key" {
		t.Errorf("ollama-cloud-web auth = %q, want api_key", kinds["ollama-cloud-web"])
	}
	if kinds["anthropic-oauth"] != "oauth" {
		t.Errorf("anthropic-oauth auth = %q, want oauth", kinds["anthropic-oauth"])
	}
	var anthFlow, anthNotice string
	var anthVersion int
	var anthAck, anthFound bool
	for _, pt := range resp.Types {
		if pt.Type == "anthropic-oauth" {
			anthFound, anthFlow, anthNotice, anthVersion, anthAck = true, pt.OAuthFlow, pt.RiskNotice, pt.RiskNoticeVersion, pt.RiskAcknowledged
		}
	}
	if !anthFound {
		t.Fatal("anthropic-oauth missing from the catalog")
	}
	if anthFlow != "paste" {
		t.Errorf("anthropic-oauth flow = %q, want paste", anthFlow)
	}
	if anthNotice == "" || anthVersion != 1 {
		t.Errorf("anthropic-oauth risk notice = %q/%d, want text/1", anthNotice, anthVersion)
	}
	if anthAck {
		t.Error("a fresh install must not report the risk notice acknowledged")
	}
}

func TestProviderTypes_CatalogRequiresSession(t *testing.T) {
	e := newAdminEnv(t)
	_ = e.bootstrap(t, "admin")

	// No session: uniform 401, never the catalog.
	w := e.call(t, http.MethodGet, "/api/admin/provider-types", "", "")
	assertStatus(t, w, http.StatusUnauthorized, "unauthenticated catalog")
}

func TestProviderTypes_DetailAccounts(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")

	// Seed two ollama accounts directly through the existing provider PUT
	// (the account-create endpoint below is the form path; the store row is
	// the same either way).
	for _, id := range []string{"acc-a", "acc-b"} {
		w := e.authedCall(t, http.MethodPut, "/api/admin/providers/"+id,
			`{"type":"ollama-cloud","name":"Account `+id+`","config":{}}`, s)
		assertStatus(t, w, http.StatusOK, "seed provider "+id)
	}
	// One codex draft WITHOUT a credential (no oauth ever completed).
	w := e.authedCall(t, http.MethodPut, "/api/admin/providers/codex-draft",
		`{"type":"codex-oauth","name":"Draft","config":{"issuer":"https://auth.openai.com"}}`, s)
	assertStatus(t, w, http.StatusOK, "seed codex draft")

	w = e.authedCall(t, http.MethodGet, "/api/admin/provider-types/ollama-cloud", "", s)
	assertStatus(t, w, http.StatusOK, "type detail")
	var resp struct {
		Settings json.RawMessage `json:"settings"`
		Accounts []struct {
			ID            string `json:"id"`
			Type          string `json:"type"`
			Name          string `json:"name"`
			HasCredential bool   `json:"has_credential"`
		} `json:"accounts"`
	}
	decodeJSON(t, w, &resp)
	if len(resp.Accounts) != 2 {
		t.Fatalf("ollama-cloud accounts = %d, want 2", len(resp.Accounts))
	}
	if resp.Accounts[0].ID != "acc-a" || resp.Accounts[1].ID != "acc-b" {
		t.Errorf("accounts not id-ascending: %+v", resp.Accounts)
	}
	for _, a := range resp.Accounts {
		if a.Type != "ollama-cloud" {
			t.Errorf("account %s has type %s, want ollama-cloud", a.ID, a.Type)
		}
	}
}

func TestProviderTypes_UnknownTypeIs404(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")

	w := e.authedCall(t, http.MethodGet, "/api/admin/provider-types/no-such", "", s)
	assertStatus(t, w, http.StatusNotFound, "unknown type detail")
}

func TestProviderTypeSettings_PutValidates(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")

	// Valid settings save.
	w := e.authedCall(t, http.MethodPut, "/api/admin/provider-types/codex-oauth/settings",
		`{"settings":{"issuer":"https://auth.openai.com"}}`, s)
	assertStatus(t, w, http.StatusOK, "settings put")

	// The SSRF guard applies on the settings path too: a private issuer is
	// a 400, never persisted.
	w = e.authedCall(t, http.MethodPut, "/api/admin/provider-types/codex-oauth/settings",
		`{"settings":{"issuer":"http://127.0.0.1:9/x"}}`, s)
	assertStatus(t, w, http.StatusBadRequest, "private issuer must be rejected")

	// An unknown type is a 404.
	w = e.authedCall(t, http.MethodPut, "/api/admin/provider-types/no-such/settings",
		`{"settings":{}}`, s)
	assertStatus(t, w, http.StatusNotFound, "unknown type settings")
}

func TestProviderTypeAccounts_Create(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")

	// The form path: name + api_key. The server mints the ID; the key
	// lands in the encrypted token store; the response DTO carries no
	// secret material.
	w := e.authedCall(t, http.MethodPost, "/api/admin/provider-types/ollama-cloud/accounts",
		`{"name":"My Ollama key","api_key":"sk-test-123"}`, s)
	assertStatus(t, w, http.StatusOK, "account create")
	var created struct {
		Account struct {
			ID            string `json:"id"`
			Type          string `json:"type"`
			Name          string `json:"name"`
			HasCredential bool   `json:"has_credential"`
		} `json:"account"`
	}
	decodeJSON(t, w, &created)
	if created.Account.ID == "" {
		t.Fatal("server must mint the account id")
	}
	if created.Account.Type != "ollama-cloud" {
		t.Errorf("created type = %s, want ollama-cloud", created.Account.Type)
	}
	if !created.Account.HasCredential {
		t.Error("account created with api_key must report has_credential")
	}
	if strings.Contains(w.Body.String(), "sk-test-123") {
		t.Errorf("response leaks the api key: %s", w.Body.String())
	}

	// The account is a providers row: the existing providers list sees it.
	w = e.authedCall(t, http.MethodGet, "/api/admin/providers", "", s)
	assertStatus(t, w, http.StatusOK, "providers list")
	var provs struct {
		Providers []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"providers"`
	}
	decodeJSON(t, w, &provs)
	found := false
	for _, p := range provs.Providers {
		if p.ID == created.Account.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("created account %s missing from providers list", created.Account.ID)
	}

	// OAuth type: creating an account needs no api_key and no own config —
	// the issuer comes from the type settings (ADR-009 decision 1: shared
	// settings first, account fields win; a draft with no own fields
	// validates against the type's issuer).
	w = e.authedCall(t, http.MethodPut, "/api/admin/provider-types/codex-oauth/settings",
		`{"settings":{"issuer":"https://auth.openai.com"}}`, s)
	assertStatus(t, w, http.StatusOK, "codex type settings")
	w = e.authedCall(t, http.MethodPost, "/api/admin/provider-types/codex-oauth/accounts",
		`{"name":"Work account"}`, s)
	assertStatus(t, w, http.StatusOK, "oauth draft create")
	var draft struct {
		Account struct {
			HasCredential bool `json:"has_credential"`
		} `json:"account"`
	}
	decodeJSON(t, w, &draft)
	if draft.Account.HasCredential {
		t.Error("a fresh oauth draft must report has_credential=false")
	}
}

func TestProviderTypeAccounts_CreateRejectsApiKeyForOAuth(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")

	// OAuth accounts authenticate via the device flow — an api_key in the
	// form is a client bug or a confused operator; refuse it.
	w := e.authedCall(t, http.MethodPost, "/api/admin/provider-types/codex-oauth/accounts",
		`{"name":"x","api_key":"whatever"}`, s)
	assertStatus(t, w, http.StatusBadRequest, "oauth account with api_key")
}

func TestProviderTypeAccounts_UnknownTypeIs404(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")
	w := e.authedCall(t, http.MethodPost, "/api/admin/provider-types/no-such/accounts", `{"name":"x"}`, s)
	assertStatus(t, w, http.StatusNotFound, "unknown type accounts")
}

func TestRoutingStrategy_GetDefaultAndRoundTrip(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")

	// Default: round_robin (ADR-009 decision 3), from storage.
	w := e.authedCall(t, http.MethodGet, "/api/admin/routing-strategy", "", s)
	assertStatus(t, w, http.StatusOK, "strategy get")
	var got struct {
		Strategy string `json:"strategy"`
	}
	decodeJSON(t, w, &got)
	if got.Strategy != "round_robin" {
		t.Errorf("default strategy = %q, want round_robin", got.Strategy)
	}

	// Round-trip fill_first, back to round_robin.
	w = e.authedCall(t, http.MethodPut, "/api/admin/routing-strategy", `{"strategy":"fill_first"}`, s)
	assertStatus(t, w, http.StatusOK, "strategy put")
	w = e.authedCall(t, http.MethodGet, "/api/admin/routing-strategy", "", s)
	decodeJSON(t, w, &got)
	if got.Strategy != "fill_first" {
		t.Errorf("round-trip strategy = %q, want fill_first", got.Strategy)
	}
}

func TestRoutingStrategy_InvalidValueIs400(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")
	for _, bad := range []string{`{"strategy":"chaos"}`, `{"strategy":""}`, `{"strategy":"ROUND_ROBIN"}`} {
		w := e.authedCall(t, http.MethodPut, "/api/admin/routing-strategy", bad, s)
		assertStatus(t, w, http.StatusBadRequest, "invalid strategy "+bad)
	}
}

func TestRoutingRules_StrategyRoundTrip(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")

	// Create a rule with an explicit strategy.
	w := e.authedCall(t, http.MethodPut, "/api/admin/routing/rule-1",
		`{"match_model":"claude-*","chain":["ollama-cloud"],"priority":0,"enabled":true,"strategy":"fill_first"}`, s)
	assertStatus(t, w, http.StatusOK, "rule put")

	w = e.authedCall(t, http.MethodGet, "/api/admin/routing", "", s)
	assertStatus(t, w, http.StatusOK, "rules list")
	var resp struct {
		Rules []struct {
			ID       string `json:"id"`
			Strategy string `json:"strategy"`
		} `json:"rules"`
	}
	decodeJSON(t, w, &resp)
	if len(resp.Rules) != 1 || resp.Rules[0].Strategy != "fill_first" {
		t.Fatalf("rule strategy round-trip: %+v, want one rule with fill_first", resp.Rules)
	}

	// An invalid strategy is a 400, not a silent inherit.
	w = e.authedCall(t, http.MethodPut, "/api/admin/routing/rule-1",
		`{"match_model":"claude-*","chain":["ollama-cloud"],"priority":0,"enabled":true,"strategy":"chaos"}`, s)
	assertStatus(t, w, http.StatusBadRequest, "invalid rule strategy")

	// Chain validation: entries must be known types or existing provider IDs.
	w = e.authedCall(t, http.MethodPut, "/api/admin/routing/rule-2",
		`{"match_model":"x","chain":["ollama-cloud","totally-unknown"],"priority":0,"enabled":true}`, s)
	assertStatus(t, w, http.StatusBadRequest, "unknown chain entry must reject")
}

func TestProviderTypeSettings_ReloadFires(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")

	before := e.keys.calls
	w := e.authedCall(t, http.MethodPut, "/api/admin/provider-types/codex-oauth/settings",
		`{"settings":{"issuer":"https://auth.openai.com"}}`, s)
	assertStatus(t, w, http.StatusOK, "settings put")
	if e.keys.calls == before {
		t.Error("settings put must fire the provider reload (live construction)")
	}
}

// The risk notice is versioned and acknowledged server-side (ADR-022): the
// console must see the version it acknowledged, and DELETE must re-open the
// gate.
func TestProviderTypes_RiskAckRoundTrip(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")

	acknowledged := func() bool {
		w := e.authedCall(t, http.MethodGet, "/api/admin/provider-types", "", s)
		assertStatus(t, w, http.StatusOK, "catalog")
		var resp struct {
			Types []struct {
				Type             string `json:"type"`
				RiskAcknowledged bool   `json:"risk_acknowledged"`
			} `json:"types"`
		}
		decodeJSON(t, w, &resp)
		for _, pt := range resp.Types {
			if pt.Type == "anthropic-oauth" {
				return pt.RiskAcknowledged
			}
		}
		t.Fatal("anthropic-oauth missing")
		return false
	}
	if acknowledged() {
		t.Fatal("fresh install reports the notice acknowledged")
	}

	w := e.authedCall(t, http.MethodPost, "/api/admin/provider-types/anthropic-oauth/risk-ack", "", s)
	assertStatus(t, w, http.StatusOK, "acknowledge")
	if !acknowledged() {
		t.Error("acknowledgement did not persist")
	}

	w = e.authedCall(t, http.MethodDelete, "/api/admin/provider-types/anthropic-oauth/risk-ack", "", s)
	assertStatus(t, w, http.StatusOK, "reset acknowledgement")
	if acknowledged() {
		t.Error("DELETE did not re-open the gate")
	}

	// A type without a notice has nothing to acknowledge; an unknown type is
	// a 404.
	w = e.authedCall(t, http.MethodPost, "/api/admin/provider-types/custom-openai/risk-ack", "", s)
	assertStatus(t, w, http.StatusBadRequest, "type without notice")
	w = e.authedCall(t, http.MethodPost, "/api/admin/provider-types/no-such/risk-ack", "", s)
	assertStatus(t, w, http.StatusNotFound, "unknown type")
}

// The Anthropic flow starts server-side without an issuer round trip: the
// response carries the authorize URL, the flow id and the redirect URI; the
// completion path rejects an unknown flow before any exchange.
func TestOAuth_AnthropicPasteStartAndComplete(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")
	w := e.authedCall(t, http.MethodPut, "/api/admin/providers/anth-1",
		`{"type":"anthropic-oauth","name":"Work","config":{}}`, s)
	assertStatus(t, w, http.StatusOK, "seed anthropic-oauth provider")

	w = e.authedCall(t, http.MethodPost, "/api/admin/providers/anth-1/oauth/start", "", s)
	assertStatus(t, w, http.StatusOK, "start paste flow")
	var start struct {
		Flow         string `json:"flow"`
		FlowID       string `json:"flow_id"`
		AuthorizeURL string `json:"authorize_url"`
		RedirectURI  string `json:"redirect_uri"`
	}
	decodeJSON(t, w, &start)
	if start.Flow != "paste" || start.FlowID == "" {
		t.Fatalf("start = %+v, want a paste flow with an id", start)
	}
	if !strings.HasPrefix(start.AuthorizeURL, "https://claude.ai/oauth/authorize?") {
		t.Errorf("authorize_url = %q", start.AuthorizeURL)
	}
	if !strings.Contains(start.AuthorizeURL, "client_id="+url.QueryEscape("9d1c250a-e61b-44d9-88ed-5944d1962f5e")) {
		t.Errorf("authorize_url missing the client id: %q", start.AuthorizeURL)
	}
	if start.RedirectURI != "http://localhost:53692/callback" {
		t.Errorf("redirect_uri = %q", start.RedirectURI)
	}

	w = e.authedCall(t, http.MethodPost, "/api/admin/providers/anth-1/oauth/complete",
		`{"flow_id":"bogus","input":"code"}`, s)
	assertStatus(t, w, http.StatusBadRequest, "unknown flow")

	w = e.authedCall(t, http.MethodPost, "/api/admin/providers/anth-1/oauth/poll", "", s)
	assertStatus(t, w, http.StatusBadRequest, "poll is not the paste completion path")
}
