package httpapi

// End-to-end vision workflow over the Anthropic surface (ADR-018): Claude
// Code pastes an image; the routed model's row is LEARNED blind; the proxy
// substitutes the image pre-dispatch (placeholder + analyze_image def) and
// answers the model's tool calls server-side. The client sees a plain,
// clean Anthropic response — paste → answer, nothing else.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
)

// visionRef computes the image ref per the spec (12 hex of SHA-256 over the
// decoded bytes) — what the placeholder shows and what the mock then quotes.
func visionRef(data string) string {
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:6])
}

// newVisionEnv = the harness env plus a second mock provider acting as the
// vision fallback, and an executor with the model-row vision lookup (the
// same wiring server.New uses).
func newVisionEnv(t *testing.T) *env {
	t.Helper()
	var authOpts []auth.Option
	st, err := storage.Open(config.StorageConfig{Path: t.TempDir() + "/test.db"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	if err := st.EnsureProvider(ctx, "mock-main", "custom", "Mock Main"); err != nil {
		t.Fatalf("seed provider row: %v", err)
	}
	if err := st.EnsureProvider(ctx, "mock-vision", "custom", "Mock Vision"); err != nil {
		t.Fatalf("seed vision provider row: %v", err)
	}

	mock := mockprovider.New(t)
	reg := provider.NewRegistry()
	reg.Register(mock.Provider("mock-main", provider.Capabilities{
		Streaming: true, Tools: true, Vision: true, Thinking: true,
	}))
	reg.Register(mock.Provider("mock-vision", provider.Capabilities{
		Streaming: true, Tools: true, Vision: true, Thinking: true,
	}))

	// The routed model's row is learned blind — what production writes after
	// the first capability_gap, stated explicitly to pin the proactive form.
	if err := st.PutModel(ctx, storage.Model{ID: "gpt-5", ProviderID: "mock-main",
		Name: "gpt-5", Capabilities: json.RawMessage(`{"vision":false}`)}); err != nil {
		t.Fatalf("seed blind model row: %v", err)
	}
	if err := st.PutModel(ctx, storage.Model{ID: "gpt-5/vl", ProviderID: "mock-vision",
		Name: "vl-wire"}); err != nil {
		t.Fatalf("seed vision model row: %v", err)
	}

	health := routing.NewHealthTracker()
	router := routing.NewRouter(routing.Config{Rules: []routing.Rule{
		{MatchModel: "gpt-5", Chain: []string{"mock-main"}, Priority: 100, Enabled: true},
	}}, routing.WithRegistry(reg), routing.WithHealth(health))
	exec := routing.NewExecutor(routing.ExecutorConfig{
		Registry:            reg,
		Health:              health,
		RetrySleeper:        func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{{ProviderID: "mock-vision", Model: "vl-wire"}},
		ModelVisionLookup: func(_ context.Context, providerID, name string) (bool, bool) {
			row, err := st.GetModelByProviderAndName(ctx, providerID, name)
			if err != nil || row.ID == "" {
				return false, false
			}
			var caps struct {
				Vision *bool `json:"vision"`
			}
			if len(row.Capabilities) == 0 || json.Unmarshal(row.Capabilities, &caps) != nil || caps.Vision == nil {
				return false, false
			}
			return *caps.Vision, true
		},
	})

	h := New(Params{Store: st, Router: router, Executor: exec})
	st.SetKeyHasher(storage.NewKeyHasher([]byte(testMasterKey)))
	keyID, key, err := auth.CreateClientKey(ctx, st, "vision-e2e")
	if err != nil {
		t.Fatalf("create client key: %v", err)
	}
	srv := httptest.NewServer(auth.NewMiddleware(st, authOpts...).Wrap(h))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { WaitForPendingCaptures(5 * time.Second) })

	return &env{
		t: t, store: st, mock: mock, reg: reg,
		router: router, exec: exec, health: health,
		srv: srv, handler: h, keyID: keyID, key: key,
	}
}

func TestMessages_ImageRequestOnBlindRowSubstitutesAndAnswersTheTool(t *testing.T) {
	e := newVisionEnv(t)
	imageB64 := base64.StdEncoding.EncodeToString([]byte("PNGDATA-paste"))
	ref := visionRef(imageB64)

	// Script order is the flow itself: bootstrap caption (vision), main
	// dispatch 1 (model asks the tool), tool answer (vision), main dispatch 2
	// (final text). The mock serves scripts in arrival order; the model name
	// in the captured requests tells the roles apart.
	e.mock.Enqueue(mockprovider.NewScript().JSONResponse(200,
		`{"choices":[{"message":{"role":"assistant","content":"a terminal window showing a compile error"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":9}}`, nil))
	e.mock.Enqueue(mockprovider.NewScript().JSONResponse(200,
		fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"analyze_image","arguments":%q}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":20}}`,
			fmt.Sprintf(`{"image_ref":"%s","prompt":"what exact error string?"}`, ref)), nil))
	e.mock.Enqueue(mockprovider.NewScript().JSONResponse(200,
		`{"choices":[{"message":{"role":"assistant","content":"the error string is E0201"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, nil))
	e.mock.Enqueue(mockprovider.NewScript().JSONResponse(200,
		`{"choices":[{"message":{"role":"assistant","content":"the build fails with E0201 in main.go"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":9}}`, nil))

	req := fmt.Sprintf(`{"model":"gpt-5","max_tokens":256,"messages":[{"role":"user","content":[
		{"type":"text","text":"what does this screenshot show?"},
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"%s"}}]}]}`, imageB64)
	status, body := e.postAnthropic(t, "/v1/messages", req)
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}

	// The client's answer is clean Anthropic: the injected tool's blocks are
	// absent, the final text rides.
	if strings.Contains(body, "analyze_image") {
		t.Errorf("the injected tool leaked into the client response: %s", body)
	}
	if !strings.Contains(body, "E0201") {
		t.Errorf("final answer missing from the response: %s", body)
	}

	// The routed model saw a placeholder + the tool def, never the image.
	requests := e.mock.Requests()
	if len(requests) != 4 {
		t.Fatalf("mock requests = %d, want 4 (caption, tool round, answer, final)", len(requests))
	}
	mainReq1 := requests[1].Body
	if strings.Contains(mainReq1, "image_url") {
		t.Errorf("the image was dispatched raw to a known-blind row")
	}
	if !strings.Contains(mainReq1, `ref `+ref) || !strings.Contains(mainReq1, `Bild 1`) {
		t.Errorf("placeholder missing from the dispatch body: %s", mainReq1[:min(400, len(mainReq1))])
	}
	if !strings.Contains(mainReq1, `"name":"analyze_image"`) {
		t.Errorf("analyze_image def missing from the dispatch body")
	}
	// The tool result carried the vision answer back to the model on the
	// re-dispatch (request 4 of the flow).
	if !strings.Contains(requests[3].Body, "E0201") {
		t.Errorf("tool_result with the vision answer missing: %.300s", requests[3].Body)
	}

	// Usage: caption + tool answer + two dispatches, honestly summed in the
	// terminal usage block.
	if u := usageInAnthropicBody(t, body); u != 167 {
		t.Errorf("client usage = %d, want 167 (7+100+10+50 prompt)", u)
	}
}

func TestMessages_ImageRequestOnBlindRowStreamsCleanly(t *testing.T) {
	e := newVisionEnv(t)
	imageB64 := base64.StdEncoding.EncodeToString([]byte("PNGDATA-stream"))
	ref := visionRef(imageB64)

	e.mock.Enqueue(mockprovider.NewScript().JSONResponse(200,
		`{"choices":[{"message":{"role":"assistant","content":"a screenshot of a failing test"},"finish_reason":"stop"}],"usage":{"prompt_tokens":6,"completion_tokens":8}}`, nil))
	e.mock.Enqueue(mockprovider.NewScript().JSONResponse(200,
		`{"choices":[{"message":{"role":"assistant","content":"the test name is TestFrob"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":4}}`, nil))

	req := fmt.Sprintf(`{"model":"gpt-5","max_tokens":256,"stream":true,"messages":[{"role":"user","content":[
		{"type":"text","text":"why is this test failing?"},
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"%s"}}]}]}`, imageB64)

	// NOTE the script: the model serves the request WITHOUT calling the tool
	// this time (the caption answered it) — one dispatch, one caption, then
	// the final answer rides the synthesized stream. Order: caption, dispatch.
	status, raw := e.postStream(t, "/v1/messages", req)
	if status != 200 {
		t.Fatalf("stream status %d: %s", status, raw)
	}
	if strings.Contains(raw, "analyze_image") {
		t.Errorf("the injected tool leaked into the streamed response")
	}
	if !strings.Contains(raw, "TestFrob") {
		t.Fatalf("final text missing from the stream: %s", raw[:min(600, len(raw))])
	}
	requests := e.mock.Requests()
	if len(requests) != 2 {
		t.Fatalf("mock requests = %d, want 2 (caption + one dispatch)", len(requests))
	}
	if !strings.Contains(requests[1].Body, `ref `+ref) {
		t.Errorf("streamed dispatch missing the image placeholder: %.200s", requests[1].Body)
	}
}

// usageInAnthropicBody extracts the terminal message.usage.input_tokens.
func usageInAnthropicBody(t *testing.T, body string) int {
	t.Helper()
	var resp struct {
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("usage parse: %v", err)
	}
	return resp.Usage.InputTokens
}
