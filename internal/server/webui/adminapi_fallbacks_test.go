package webui

// ADR-010 admin surface contract tests, derived from the ADR + its
// 2026-09-21 amendment (automatic capabilities), not implementation
// behavior:
//
//   - GET/PUT /api/admin/capability-fallbacks: three lists (vision,
//     web_search, web_fetch). Spec change 2026-09-21: vision entries are
//     MODEL IDs (routing identities) validated against the models table —
//     provider accounts/types are refused; web lists only accept
//     web_tool-kind accounts; a save pushes ReloadFallbacks; wire shape
//     never carries nulls.
//   - GET/PUT /api/admin/model-sync: config round-trip (enabled +
//     interval), non-positive interval is a 400.
//   - POST /api/admin/model-sync/run: unwired sync answers 404, wired sync
//     returns the run summary.

import (
	"context"
	"net/http"
	"testing"

	"github.com/tubeof/proxyllm/internal/storage"
)

// seedFallbackModel stores one model row on the custom-openai account (the
// account row goes directly into the store: a local base_url would trip
// the SSRF guard, and the validation under test reads store rows).
func seedFallbackModel(t *testing.T, e *adminEnv, id string) {
	t.Helper()
	if err := e.st.PutProvider(context.Background(), storage.Provider{
		ID: "acc-llm", Type: "custom-openai", Name: "llm", Config: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.PutModel(context.Background(), storage.Model{
		ID: id, ProviderID: "acc-llm", Name: "wire-" + id,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityFallbacks_RoundTrip(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")

	// A firecrawl account for the web lists; the vision list names a MODEL.
	seedFallbackModel(t, e, "ollamacloud/vision-model")
	w := e.authedCall(t, http.MethodPost, "/api/admin/provider-types/firecrawl/accounts",
		`{"name":"fc","api_key":"fc-key-1"}`, s)
	assertStatus(t, w, http.StatusOK, "create firecrawl account")
	var created struct {
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	decodeJSON(t, w, &created)

	body := `{"vision":["ollamacloud/vision-model"],"web_search":["` + created.Account.ID + `"],"web_fetch":["` + created.Account.ID + `"]}`
	w = e.authedCall(t, http.MethodPut, "/api/admin/capability-fallbacks", body, s)
	assertStatus(t, w, http.StatusOK, "save fallbacks")

	w = e.authedCall(t, http.MethodGet, "/api/admin/capability-fallbacks", "", s)
	assertStatus(t, w, http.StatusOK, "load fallbacks")
	var got struct {
		Vision    []string `json:"vision"`
		WebSearch []string `json:"web_search"`
		WebFetch  []string `json:"web_fetch"`
	}
	decodeJSON(t, w, &got)
	if len(got.Vision) != 1 || got.Vision[0] != "ollamacloud/vision-model" {
		t.Errorf("vision list = %v", got.Vision)
	}
	if len(got.WebSearch) != 1 || got.WebSearch[0] != created.Account.ID {
		t.Errorf("web_search list = %v", got.WebSearch)
	}
}

func TestCapabilityFallbacks_UnsetReadsEmptyArrays(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")

	w := e.authedCall(t, http.MethodGet, "/api/admin/capability-fallbacks", "", s)
	assertStatus(t, w, http.StatusOK, "load fallbacks")
	var got struct {
		Vision    []string `json:"vision"`
		WebSearch []string `json:"web_search"`
		WebFetch  []string `json:"web_fetch"`
	}
	decodeJSON(t, w, &got)
	if got.Vision == nil || got.WebSearch == nil || got.WebFetch == nil {
		t.Errorf("unset lists must read as [] not null: %+v", got)
	}
}

// Spec change 2026-09-21: a provider TYPE entry (the v1.0.2 shape) is no
// longer a valid vision fallback — entries are model IDs.
func TestCapabilityFallbacks_RefusesProviderTypeAsVision(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")

	w := e.authedCall(t, http.MethodPut, "/api/admin/capability-fallbacks",
		`{"vision":["firecrawl"],"web_search":[],"web_fetch":[]}`, s)
	assertStatus(t, w, http.StatusBadRequest, "provider type as vision entry")
}

func TestCapabilityFallbacks_RefusesLLMAccountInWebList(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")

	// A custom-openai account is not a web-tool backend. Local base_url
	// would trip the SSRF guard, so the account row goes directly into the
	// store (the validation under test reads store rows, not the catalog).
	if err := e.st.PutProvider(context.Background(), storage.Provider{
		ID: "acc-llm", Type: "custom-openai", Name: "llm", Config: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	w := e.authedCall(t, http.MethodPut, "/api/admin/capability-fallbacks",
		`{"vision":[],"web_search":["acc-llm"],"web_fetch":[]}`, s)
	assertStatus(t, w, http.StatusBadRequest, "llm account in web-tool list")
}

func TestCapabilityFallbacks_UnknownVisionEntryIs400(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")

	w := e.authedCall(t, http.MethodPut, "/api/admin/capability-fallbacks",
		`{"vision":["no-such-model"],"web_search":[],"web_fetch":[]}`, s)
	assertStatus(t, w, http.StatusBadRequest, "unknown vision entry")
}

func TestModelSync_ConfigRoundTrip(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")

	w := e.authedCall(t, http.MethodGet, "/api/admin/model-sync", "", s)
	assertStatus(t, w, http.StatusOK, "load sync config")
	var cfg struct {
		Enabled       bool `json:"enabled"`
		IntervalHours int  `json:"interval_hours"`
	}
	decodeJSON(t, w, &cfg)
	if !cfg.Enabled || cfg.IntervalHours != 24 {
		t.Errorf("defaults = %+v, want enabled + 24h", cfg)
	}

	w = e.authedCall(t, http.MethodPut, "/api/admin/model-sync",
		`{"enabled":false,"interval_hours":6}`, s)
	assertStatus(t, w, http.StatusOK, "save sync config")

	w = e.authedCall(t, http.MethodGet, "/api/admin/model-sync", "", s)
	assertStatus(t, w, http.StatusOK, "reload sync config")
	decodeJSON(t, w, &cfg)
	if cfg.Enabled || cfg.IntervalHours != 6 {
		t.Errorf("saved config = %+v, want disabled + 6h", cfg)
	}
}

func TestModelSync_NonPositiveIntervalIs400(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")

	w := e.authedCall(t, http.MethodPut, "/api/admin/model-sync",
		`{"interval_hours":0}`, s)
	assertStatus(t, w, http.StatusBadRequest, "interval 0")
}

func TestModelSyncRun_UnwiredIs404(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")

	w := e.authedCall(t, http.MethodPost, "/api/admin/model-sync/run", "", s)
	assertStatus(t, w, http.StatusNotFound, "unwired sync run")
}

func TestModelSyncRun_WiredReturnsSummary(t *testing.T) {
	e := newAdminEnv(t)
	s := e.bootstrap(t, "admin")

	// Wire the sync the way server.New does (the AdminAPI field is direct).
	e.api.syncNow = func(_ context.Context) (storage.ModelSyncState, error) {
		return storage.ModelSyncState{RanAt: 42, Providers: []storage.ModelSyncProviderState{
			{ProviderID: "acc-1", Added: 2},
		}}, nil
	}

	w := e.authedCall(t, http.MethodPost, "/api/admin/model-sync/run", "", s)
	assertStatus(t, w, http.StatusOK, "wired sync run")
	var resp struct {
		LastRun storage.ModelSyncState `json:"last_run"`
	}
	decodeJSON(t, w, &resp)
	if resp.LastRun.RanAt != 42 || len(resp.LastRun.Providers) != 1 || resp.LastRun.Providers[0].Added != 2 {
		t.Errorf("run summary = %+v", resp.LastRun)
	}
}

func TestCapabilityFallbacks_RequireAdmin(t *testing.T) {
	e := newAdminEnv(t)
	_ = e.bootstrap(t, "admin")

	w := e.call(t, http.MethodGet, "/api/admin/capability-fallbacks", "", "")
	assertStatus(t, w, http.StatusUnauthorized, "no session")
}
