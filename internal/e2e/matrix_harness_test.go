package e2e

// Conformance-matrix harness: the official client SDKs and raw HTTP drive a
// fully assembled proxy whose upstreams are MockServer wire simulators. Each
// cell pairs one CLIENT protocol with one UPSTREAM protocol, so the matrix
// proves every codec pair, not only the default one.
//
// The upstreams speak the provider's real wire, produced by MockServer's
// built-in LLM mocking (OPENAI, OPENAI_RESPONSES, ANTHROPIC, GEMINI, OLLAMA),
// including streaming framing, tool calls and fault profiles. No provider key
// is involved, and no hand-written fake stands in for a provider.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	openaiclient "github.com/openai/openai-go/v3"
	openaioption "github.com/openai/openai-go/v3/option"

	"github.com/tubeof/proxyllm/internal/auth"
	"github.com/tubeof/proxyllm/internal/config"
	"github.com/tubeof/proxyllm/internal/provider"
	anthropicprovider "github.com/tubeof/proxyllm/internal/provider/anthropic"
	"github.com/tubeof/proxyllm/internal/provider/codex"
	"github.com/tubeof/proxyllm/internal/provider/custom"
	geminiprovider "github.com/tubeof/proxyllm/internal/provider/gemini"
	"github.com/tubeof/proxyllm/internal/provider/ollamacloud"
	"github.com/tubeof/proxyllm/internal/provider/ollamanative"
	"github.com/tubeof/proxyllm/internal/provider/openaidecisions"
	"github.com/tubeof/proxyllm/internal/provider/openairesponses"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/server/httpapi"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/testutil/mockserver"
	"github.com/tubeof/proxyllm/internal/tools/firecrawl"
)

// upstream is one provider row under test: its type decides the wire the
// proxy speaks to it, and BaseURL points at the simulator.
type upstream struct {
	ID      string
	Type    string // custom-openai | openai-responses | anthropic | ollama-native | ollama-cloud | gemini | openai-decisions | codex-oauth
	BaseURL string
	Caps    provider.Capabilities
}

// matrixEnv assembles the proxy with one provider per declared upstream and a
// routing rule whose model name is the upstream ID.
type matrixEnv struct {
	ms      *mockserver.Server
	store   *storage.Store
	srv     *httptest.Server
	key     string
	exec    *routing.Executor
	health  *routing.HealthTracker
	openai  openaiclient.Client
	anthrop anthropic.Client
	// wire tees the client-facing response bytes at the proxy boundary (the
	// schema validation reads them; see matrix_schema_test.go).
	wire *wireCapture
}

// allCaps is the capability set a simulator-backed provider advertises: every
// surface the matrix exercises.
func allCaps() provider.Capabilities {
	return provider.Capabilities{
		Streaming: true, Tools: true, Vision: true,
		WebSearch: true, WebFetch: true, CodeExecution: true, Thinking: true,
	}
}

// newMatrixEnv assembles the proxy against MockServer. The caller installs
// the expectations afterwards (after a Reset, which the harness performs).
func newMatrixEnv(t *testing.T, ups ...upstream) *matrixEnv {
	t.Helper()
	return buildMatrixEnv(t, nil, ups...)
}

// newMatrixEnvEmulated assembles the same proxy with the web-tool emulator
// pool pointed at MockServer too, so the emulated web_search/web_fetch flow
// runs entirely offline: MockServer also serves the Firecrawl v2 wire as the
// web-tool backend.
func newMatrixEnvEmulated(t *testing.T, ups ...upstream) *matrixEnv {
	t.Helper()
	return buildMatrixEnv(t, func(base string) routing.Emulator {
		emu := firecrawl.NewEmulator(base, "simulated-key", nil)
		return routing.NewEmulatorPool([]routing.Emulator{emu}, []routing.Emulator{emu})
	}, ups...)
}

// buildMatrixEnv assembles the proxy; emuFactory (nil for none) receives the
// MockServer base URL so the web-tool backend and the expectations share one
// instance.
func buildMatrixEnv(t *testing.T, emuFactory func(base string) routing.Emulator, ups ...upstream) *matrixEnv {
	t.Helper()
	ms := mockserver.New(t)
	ms.Reset(t)
	var emu routing.Emulator
	if emuFactory != nil {
		emu = emuFactory(ms.URL)
	}

	ctx := context.Background()
	st, err := storage.Open(config.StorageConfig{Path: t.TempDir() + "/test.db"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// The secret envelope the store requires for token rows (ADR-007): a
	// codex-oauth upstream persists its OAuth token triple.
	envelope, err := storage.NewEnvelope(make([]byte, 32))
	if err != nil {
		t.Fatalf("test envelope: %v", err)
	}
	st.SetEnvelope(envelope)

	reg := provider.NewRegistry()
	rules := make([]routing.Rule, 0, len(ups))
	for _, u := range ups {
		if u.BaseURL == "" {
			u.BaseURL = ms.URL
		}
		if u.Caps == (provider.Capabilities{}) {
			u.Caps = allCaps()
		}
		if err := st.EnsureProvider(ctx, u.ID, u.Type, u.ID); err != nil {
			t.Fatalf("seed provider %s: %v", u.ID, err)
		}
		reg.Register(buildUpstream(ctx, u, st))
		rules = append(rules, routing.Rule{
			MatchModel: u.ID, Chain: []string{u.ID}, Priority: 100, Enabled: true,
		})
	}

	health := routing.NewHealthTracker()
	router := routing.NewRouter(routing.Config{Rules: rules},
		routing.WithRegistry(reg), routing.WithHealth(health))
	exec := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     reg,
		Health:       health,
		Emulator:     emu,
		RetrySleeper: func(time.Duration) {},
		// The shipped learning contract rides with the assembly (the server
		// wires learnCapabilityGap, which persists vision:false through
		// MergeLearnedCapability on every capability_gap). The harness keeps
		// the write inline — synchronous, no goroutine — so a cell can assert
		// the learned row deterministically under -race.
		OnCapabilityGap: func(entry routing.ChainEntry) {
			learnCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := st.MergeLearnedCapability(learnCtx, entry.ProviderID, entry.Model, "vision", false); err != nil {
				t.Logf("capability-gap learning write failed: %v", err)
			}
		},
	})
	// The shipped claim mapping rides with the assembly, as the server wires
	// it (loadClientWebTools): a client function tool the mapping names is
	// the proxy's to serve through the emulator pool.
	h := httpapi.New(httpapi.Params{Store: st, Router: router, Executor: exec,
		ClientWebTools: httpapi.DefaultClientWebTools()})

	st.SetKeyHasher(storage.NewKeyHasher([]byte(testMasterKey)))
	_, key, err := auth.CreateClientKey(ctx, st, "matrix-test")
	if err != nil {
		t.Fatalf("create client key: %v", err)
	}

	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")

	wire := &wireCapture{}
	srv := httptest.NewServer(auth.NewMiddleware(st).Wrap(wire.Wrap(h)))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { httpapi.WaitForPendingCaptures(5 * time.Second) })

	return &matrixEnv{
		ms: ms, store: st, srv: srv, key: key, exec: exec, health: health, wire: wire,
		openai: openaiclient.NewClient(
			openaioption.WithBaseURL(srv.URL+"/v1"),
			openaioption.WithAPIKey(key),
			openaioption.WithMaxRetries(0),
		),
		anthrop: anthropic.NewClient(
			option.WithBaseURL(srv.URL),
			option.WithAPIKey(key),
			option.WithMaxRetries(0),
		),
	}
}

// The pre-provisioned codex-oauth account's token values: the harness plays an
// operator whose login predates the run and whose access token has EXPIRED,
// so every codex-oauth cell must rotate against the issuer (MockServer, under
// the same base URL) before the first request rides.
const (
	codexOAuthSeedAccess  = "codex-seeded-expired-access"
	codexOAuthSeedRefresh = "codex-seeded-refresh"
)

// buildUpstream maps an upstream spec onto the production provider type.
func buildUpstream(ctx context.Context, u upstream, st *storage.Store) provider.Provider {
	const apiKey = "simulated-key"
	switch u.Type {
	case "custom-openai":
		// custom-openai's client carries "/chat/completions", so its base URL
		// is the API root (…/v1).
		return custom.New(u.ID, u.BaseURL+"/v1", apiKey, u.Caps)
	case "openai-responses":
		return openairesponses.New(u.ID, u.BaseURL, apiKey, u.Caps)
	case "anthropic":
		return anthropicprovider.New(u.ID, u.BaseURL, apiKey, u.Caps)
	case "ollama-native":
		return ollamanative.New(u.ID, u.BaseURL, "", u.Caps)
	case "openai-decisions":
		return openaidecisions.New(u.ID, u.BaseURL, apiKey, u.Caps)
	case "ollama-cloud":
		return ollamacloud.New(u.ID, u.BaseURL, apiKey, u.Caps)
	case "gemini":
		return geminiprovider.New(u.ID, u.BaseURL, apiKey, u.Caps)
	case "codex-oauth":
		// The account is pre-provisioned as a previous operator login: an
		// EXPIRED access token plus its refresh token, seeded before the
		// provider is built (HasCredential is cached at construction), so
		// the first request must rotate against the issuer — MockServer
		// also plays it at {BaseURL}/oauth/token, scripted by the cell.
		expired := time.Now().Add(-time.Minute).Unix()
		if err := st.PutProviderTokens(ctx,
			storage.ProviderToken{ProviderID: u.ID, Kind: codex.KindAccessToken, Secret: []byte(codexOAuthSeedAccess), ExpiresAt: &expired},
			storage.ProviderToken{ProviderID: u.ID, Kind: codex.KindRefreshToken, Secret: []byte(codexOAuthSeedRefresh)},
		); err != nil {
			panic("matrix: seed codex-oauth tokens: " + err.Error())
		}
		return codex.New(codex.ProviderConfig{
			ID: u.ID, BaseURL: u.BaseURL, Capabilities: u.Caps,
			Auth: codex.NewAuthClient(codex.AuthConfig{
				Issuer: u.BaseURL, Store: st, ProviderID: u.ID,
			}),
		})
	default:
		panic("matrix: unknown upstream type " + u.Type)
	}
}

// expectation builds one MockServer LLM expectation. completion is the
// provider-neutral completion object documented by MockServer
// (text/stopReason/toolCalls/streaming).
func expectation(provider, model, path string, completion map[string]any) map[string]any {
	return map[string]any{
		"httpRequest": map[string]any{"method": "POST", "path": path},
		"httpLlmResponse": map[string]any{
			"provider":   provider,
			"model":      model,
			"completion": completion,
		},
	}
}

// chatCompletion is the provider-neutral completion body for a text answer.
func chatCompletion(text string, streaming bool) map[string]any {
	return map[string]any{"text": text, "streaming": streaming}
}

// upstreamJSON decodes the JSON body of one recorded upstream request.
func upstreamJSON(t *testing.T, r mockserver.Recorded) map[string]any {
	t.Helper()
	if len(r.Body.JSON) == 0 {
		t.Fatalf("upstream request %s %s carried no JSON body", r.Method, r.Path)
	}
	var out map[string]any
	if err := json.Unmarshal(r.Body.JSON, &out); err != nil {
		t.Fatalf("upstream body is not JSON: %v (%s)", err, r.Body.JSON)
	}
	return out
}
