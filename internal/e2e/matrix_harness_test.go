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
	"github.com/tubeof/proxyllm/internal/provider/custom"
	geminiprovider "github.com/tubeof/proxyllm/internal/provider/gemini"
	"github.com/tubeof/proxyllm/internal/provider/ollamacloud"
	"github.com/tubeof/proxyllm/internal/provider/ollamanative"
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
	Type    string // custom-openai | openai-responses | anthropic | ollama-native | ollama-cloud | gemini
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
	openai  openaiclient.Client
	anthrop anthropic.Client
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
		reg.Register(buildUpstream(u))
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
	})
	h := httpapi.New(httpapi.Params{Store: st, Router: router, Executor: exec})

	st.SetKeyHasher(storage.NewKeyHasher([]byte(testMasterKey)))
	_, key, err := auth.CreateClientKey(ctx, st, "matrix-test")
	if err != nil {
		t.Fatalf("create client key: %v", err)
	}

	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")

	srv := httptest.NewServer(auth.NewMiddleware(st).Wrap(h))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { httpapi.WaitForPendingCaptures(5 * time.Second) })

	return &matrixEnv{
		ms: ms, store: st, srv: srv, key: key,
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

// buildUpstream maps an upstream spec onto the production provider type.
func buildUpstream(u upstream) provider.Provider {
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
	case "ollama-cloud":
		return ollamacloud.New(u.ID, u.BaseURL, apiKey, u.Caps)
	case "gemini":
		return geminiprovider.New(u.ID, u.BaseURL, apiKey, u.Caps)
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
