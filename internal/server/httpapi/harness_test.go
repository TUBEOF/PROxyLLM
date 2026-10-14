package httpapi

// Test harness for the client-facing API surfaces: a full
// client → auth → httpapi → Router → Executor → mock-upstream round trip
// over real HTTP. RED tests derive exclusively from the wire contracts,
// golden fixtures under testdata/golden/, and the routing/compat specs —
// never from implementation behavior.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/auth"
	"github.com/tubeof/proxyllm/internal/config"
	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/testutil/mockprovider"
	ttestutil "github.com/tubeof/proxyllm/internal/translate/testutil"
)

// testMasterKey is a fixed master key for the keyed-MAC client-key hasher in
// tests: production generates or reads one, and these tests need the production
// WIRING, not production key material.
const testMasterKey = "master-key-material-for-tests"

// env is one assembled proxy-under-test: store, mock upstreams, router,
// executor, and an httptest server serving the httpapi handler behind the
// auth middleware. Tests shape behavior through the routing rules and the
// mock scripts they enqueue.
type env struct {
	t      *testing.T
	store  *storage.Store
	mock   *mockprovider.Server
	reg    provider.Registry
	router *routing.Router
	exec   *routing.Executor
	health *routing.HealthTracker
	srv    *httptest.Server
	keyID  string
	key    string // display-once client key (Bearer / x-api-key)
}

// newEnv assembles the default environment: one mock upstream provider
// ("mock-main", all capabilities, streaming) wired via one routing rule
// "gpt-5" → ["mock-main"]. Tests mutate via the returned env and the
// mockprovider scripts.
func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWithRules(t, routing.Rule{
		MatchModel: "gpt-5",
		Chain:      []string{"mock-main"},
		Priority:   100,
		Enabled:    true,
	})
}

// newEnvWithRules assembles the environment with explicit routing rules
// (mock-main registered as the sole provider).
func newEnvWithRules(t *testing.T, rules ...routing.Rule) *env {
	t.Helper()
	return newEnvWithAuthOpts(t, nil, rules...)
}

// newEnvAuthDisabled assembles the environment with client-key auth
// disabled (PROXYLLM_AUTH_DISABLED opt-out): requests
// must pass with NO key at all. Routes claude-sonnet-5 like newClaudeEnv so
// the golden Anthropic fixture requests resolve.
func newEnvAuthDisabled(t *testing.T) *env {
	t.Helper()
	return newEnvWithAuthOpts(t, []auth.Option{auth.WithAuthDisabled()},
		routing.Rule{
			MatchModel: "claude-sonnet-5",
			Chain:      []string{"mock-main"},
			Priority:   100,
			Enabled:    true,
		})
}

// newEnvWithAuthOpts assembles the environment with explicit routing rules
// (mock-main registered as the sole provider) and auth-middleware options.
func newEnvWithAuthOpts(t *testing.T, authOpts []auth.Option, rules ...routing.Rule) *env {
	t.Helper()
	st, err := storage.Open(config.StorageConfig{Path: t.TempDir() + "/test.db"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Provider row (FK target for models) via the exported seeding path.
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

	// One shared HealthTracker: the router gates on it and the executor
	// reports into it — decoupled instances would mask health-gated
	// failover bugs.
	health := routing.NewHealthTracker()
	router := routing.NewRouter(routing.Config{Rules: rules},
		routing.WithRegistry(reg), routing.WithHealth(health),
		// ADR-011 implicit model routes: the models-table row names the
		// provider — same wiring as server.New.
		routing.WithModelLookup(func(ctx context.Context, modelID string) (string, bool) {
			row, err := st.GetModel(ctx, modelID)
			if err != nil || row.ProviderID == "" {
				return "", false
			}
			return row.ProviderID, true
		}))
	exec := routing.NewExecutor(routing.ExecutorConfig{
		Registry: reg,
		Health:   health,
		// Deterministic: no real sleeping in tests.
		RetrySleeper: func(time.Duration) {},
	})

	h := New(Params{Store: st, Router: router, Executor: exec})

	// Production parity: main derives the client-key hasher from
	// the master key; without it this store would issue legacy Argon2id rows and
	// every request in this suite would pay a KDF that no production request
	// pays.
	st.SetKeyHasher(storage.NewKeyHasher([]byte(testMasterKey)))
	keyID, key, err := auth.CreateClientKey(ctx, st, "httpapi-test")
	if err != nil {
		t.Fatalf("create client key: %v", err)
	}

	srv := httptest.NewServer(auth.NewMiddleware(st, authOpts...).Wrap(h))
	t.Cleanup(srv.Close)

	// Payload capture runs OFF the request path (see capturesInFlight), so it can
	// outlive the request; cleanups are LIFO, so registering this last makes it run
	// first — before the store closes and before TempDir removes its directory.
	t.Cleanup(func() { WaitForPendingCaptures(5 * time.Second) })

	return &env{
		t: t, store: st, mock: mock, reg: reg,
		router: router, exec: exec, health: health,
		srv: srv, keyID: keyID, key: key,
	}
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

// post sends one authenticated JSON POST and returns status + body.
func (e *env) post(t *testing.T, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.key)
	return e.do(t, req)
}

// get sends one authenticated GET.
func (e *env) get(t *testing.T, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.key)
	return e.do(t, req)
}

// do executes req and returns status + body.
func (e *env) do(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

// postStream sends one authenticated streaming POST and returns status +
// the raw SSE body (all frames, read to completion or ctx cancel).
func (e *env) postStream(t *testing.T, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	status := resp.StatusCode
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return status, string(b)
}

// golden loads one golden fixture (relative to testdata/golden/).
func golden(t *testing.T, rel string) []byte {
	t.Helper()
	return ttestutil.LoadGolden(t, rel)
}

// jsonField extracts one top-level field as raw JSON; fails the test when
// absent.
func jsonField(t *testing.T, body, field string) json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("body not a JSON object: %v (%s)", err, body)
	}
	v, ok := m[field]
	if !ok {
		t.Fatalf("field %q missing in %s", field, body)
	}
	return v
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

// sseDataFrames extracts the data payloads of an SSE body (one per frame).
func sseDataFrames(body string) []string {
	var out []string
	for _, frame := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			if after, ok := strings.CutPrefix(line, "data: "); ok {
				out = append(out, after)
			}
		}
	}
	return out
}
