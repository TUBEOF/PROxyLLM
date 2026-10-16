package smoke

// ENV-gated live smoke: the Claude Code auxiliary-request path against the
// LIVE ollama.com endpoint — no mocks anywhere in this file.
//
// Claude Code fires auxiliary requests under versioned Claude IDs its own
// release pins (claude-sonnet-4-6, claude-haiku-5-5 — the WebFetch apply
// call, the auto-mode classifier). The operator maps each family with a
// wildcard alias entry (claude-haiku-*); this smoke pins that the whole
// production stack (router with wildcard aliases → executor → ollama-cloud
// provider → live upstream → Anthropic-shaped response) serves such an ID
// with a real answer, and that a name nothing maps answers the reference's
// model-not-found envelope (404 + not_found_error + "model: <id>") — the
// shape Claude Code classifies before it runs its own fallback.
//
// Requires PROXYLLM_SMOKE=1 and PROXYLLM_SMOKE_OLLAMA_CLOUD_KEY.
// PROXYLLM_SMOKE_OLLAMA_CLOUD_MODEL overrides the target model (default
// gpt-oss:120b, the operator's cheap serving model).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/auth"
	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/provider/ollamacloud"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/server/httpapi"
	"github.com/tubeof/proxyllm/internal/storage"
)

// smokeClaudeFamilyEnv assembles the production stack over the live
// upstream: one ollama-cloud account, one rule serving the target model, and
// the operator's wildcard family aliases.
func smokeClaudeFamilyEnv(t *testing.T, target string) (*httptest.Server, *storage.Store) {
	t.Helper()
	key := smokeEnv(t, "PROXYLLM_SMOKE_OLLAMA_CLOUD_KEY", "")
	base := smokeEnv(t, "PROXYLLM_SMOKE_OLLAMA_CLOUD_URL", "https://ollama.com")

	st := newSmokeStore(t)
	ctx := context.Background()
	if err := st.EnsureProvider(ctx, "smoke-ollama", "ollama-cloud", "Smoke Ollama Cloud"); err != nil {
		t.Fatalf("seed provider row: %v", err)
	}
	if err := st.PutModel(ctx, storage.Model{
		ID: target, ProviderID: "smoke-ollama", Name: "gpt-oss:120b",
	}); err != nil {
		t.Fatalf("seed model row: %v", err)
	}

	reg := provider.NewRegistry()
	reg.Register(ollamacloud.New("smoke-ollama", base, key, provider.Capabilities{
		Streaming: true, Tools: true,
	}))
	health := routing.NewHealthTracker()
	router := routing.NewRouter(routing.Config{
		Rules: []routing.Rule{
			{MatchModel: target, Chain: []string{"smoke-ollama"}, Priority: 100, Enabled: true},
		},
		Aliases: map[string]string{"claude-sonnet-*": target, "claude-haiku-*": target},
	}, routing.WithRegistry(reg), routing.WithHealth(health),
		routing.WithModelLookup(func(ctx context.Context, modelID string) (string, bool) {
			row, err := st.GetModel(ctx, modelID)
			if err != nil || row.ProviderID == "" {
				return "", false
			}
			return row.ProviderID, true
		}))
	exec := routing.NewExecutor(routing.ExecutorConfig{
		Registry: reg, Health: health,
		RetrySleeper: func(time.Duration) {},
	})

	h := httpapi.New(httpapi.Params{Store: st, Router: router, Executor: exec})
	srv := httptest.NewServer(auth.NewMiddleware(st, auth.WithAuthDisabled()).Wrap(h))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { httpapi.WaitForPendingCaptures(5 * time.Second) })
	return srv, st
}

// TestSmoke_ClaudeFamilyAliasServesAuxiliaryRequests pins the wildcard entry
// against the LIVE upstream: claude-haiku-5-5 (the auto-mode classifier's
// pinned ID) resolves through the claude-haiku-* alias and gets a real answer.
func TestSmoke_ClaudeFamilyAliasServesAuxiliaryRequests(t *testing.T) {
	if !smokeEnabled(t) {
		return
	}
	target := smokeEnv(t, "PROXYLLM_SMOKE_OLLAMA_CLOUD_MODEL", "ollama-cloud/gpt-oss:120b")
	srv, st := smokeClaudeFamilyEnv(t, target)

	body := `{"model":"claude-haiku-5-5","max_tokens":1024,"messages":[{"role":"user","content":"Reply with exactly: hello"}]}`
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("live round trip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the family alias did not reach the live upstream", resp.StatusCode)
	}
	var payload struct {
		Content []json.RawMessage `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if len(payload.Content) == 0 {
		t.Error("live answer carries no content parts")
	}

	// The log is the evidence the request really took the family path (and
	// not some implicit route): an alias event names the client-facing ID.
	logs, err := st.ListRequestLogs(context.Background(), 10)
	if err != nil || len(logs) == 0 {
		t.Fatalf("request log unreadable (%v) or empty", err)
	}
	var trace struct {
		Events []struct {
			Type string `json:"type"`
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"events"`
	}
	if err := json.Unmarshal(logs[0].RouteTrace, &trace); err != nil {
		t.Fatalf("route_trace decode: %v", err)
	}
	var aliased bool
	for _, ev := range trace.Events {
		if ev.Type == "alias" && ev.From == "claude-haiku-5-5" && ev.To == target {
			aliased = true
		}
		if ev.Type == "failed" {
			t.Errorf("route_trace carries a failed event: %+v", ev)
		}
	}
	if !aliased {
		t.Errorf("no alias event from claude-haiku-5-5 in route_trace: %s", logs[0].RouteTrace)
	}
	t.Logf("claude-haiku-5-5 served via the claude-haiku-* alias %q on the live upstream", target)
}

// TestSmoke_ModelNotFoundContract pins the 404 a name nothing maps answers:
// the reference's not_found_error envelope whose message names the model —
// the classification Claude Code requires before its own model fallback can
// fire.
func TestSmoke_ModelNotFoundContract(t *testing.T) {
	if !smokeEnabled(t) {
		return
	}
	target := smokeEnv(t, "PROXYLLM_SMOKE_OLLAMA_CLOUD_MODEL", "ollama-cloud/gpt-oss:120b")
	srv, _ := smokeClaudeFamilyEnv(t, target)

	body := `{"model":"claude-nonexistent-9-9","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if payload.Error.Type != "not_found_error" {
		t.Errorf("error.type = %q, want not_found_error", payload.Error.Type)
	}
	if !strings.Contains(payload.Error.Message, "model: claude-nonexistent-9-9") {
		t.Errorf("error.message = %q, want it to name the model as \"model: <id>\"", payload.Error.Message)
	}
}
