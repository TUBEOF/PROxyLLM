package server

// Capability-fallback wiring tests (2026-09-21 automatic capabilities):
// the stored vision_fallback KV holds MODEL IDs; New resolves them into
// executor chain entries (provider + wire name), the admin fallback write
// live-swaps the resolved entries, and a real capability_gap upstream
// rejection (a) serves the request from the fallback model and (b) learns
// vision=false into the gap model's row — respecting the pinned flag.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/testutil/mockprovider"
)

// gapRejectedScript scripts a 400 image-not-supported rejection (the
// upstream capability_gap shape pinned in internal/routing/capabilitygap_test.go).
func gapRejectedScript() *mockprovider.Script {
	return mockprovider.NewScript().JSONResponse(http.StatusBadRequest,
		`{"error":{"code":"unsupported_content","message":"image input is not supported by this model"}}`, nil)
}

// chatOKScript scripts one successful OpenAI-compatible completion.
func chatOKScript(model, content string) *mockprovider.Script {
	return mockprovider.NewScript().JSONResponse(http.StatusOK,
		fmt.Sprintf(`{"id":"c","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			model, content), nil)
}

// capabilityFallbackChat issues one image-bearing chat request through the
// proxy surface. The conversation carries earlier turns with distinctive
// markers, so a test can prove what the vision model did NOT receive.
func capabilityFallbackChat(t *testing.T, ts *httptest.Server, key string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"ollamacloud/chat-model","messages":[`+
			`{"role":"user","content":"EARLIER-TURN-MARKER"},`+
			`{"role":"assistant","content":"EARLIER-REPLY-MARKER"},`+
			`{"role":"user","content":[`+
			`{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,x"}}]}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := ensureBodyClosed(t, resp)
	return resp.StatusCode, body
}

// saveFallbacks writes the vision list through the admin API — the same
// path the WebUI uses, so the write triggers the ReloadFallbacks hook.
func saveFallbacks(t *testing.T, ts *httptest.Server, vision string, login wiringLogin) {
	t.Helper()
	body := fmt.Sprintf(`{"vision":%s,"web_search":[],"web_fetch":[]}`, vision)
	resp := adminCall(t, ts, http.MethodPut, "/api/admin/capability-fallbacks", body, login)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save fallbacks: status %d, body %s", resp.StatusCode, ensureBodyClosed(t, resp))
	}
	_ = ensureBodyClosed(t, resp)
}

// TestServer_CapabilityFallbackCaptionsImageAndRetriesTheChain: the stored
// vision_fallback list names a MODEL that DESCRIBES images (ADR-014). A real
// image rejection on the routed chain is rescued by that model's description,
// and — the point of the design — the request is answered by the ROUTED model.
// The vision model is never handed the conversation, and the gap is learned
// into the primary model's row.
func TestServer_CapabilityFallbackCaptionsImageAndRetriesTheChain(t *testing.T) {
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
	if err := st.EnsureProvider(ctx, "mock-fb", "custom", "Mock Fallback"); err != nil {
		t.Fatalf("ensure provider: %v", err)
	}
	// The fallback model row: prefixed routing ID, bare wire name.
	if err := st.PutModel(ctx, storage.Model{
		ID: "ollamacloud/fallback-vision", ProviderID: "mock-fb", Name: "fallback-vision",
	}); err != nil {
		t.Fatalf("put model: %v", err)
	}
	// The primary model row (the one that will learn vision=false).
	if err := st.PutModel(ctx, storage.Model{
		ID: "ollamacloud/chat-model", ProviderID: "mock-main", Name: "chat-model",
	}); err != nil {
		t.Fatalf("put model: %v", err)
	}
	if err := st.PutRoutingRule(ctx, storage.RoutingRule{
		ID: "r1", MatchModel: "ollamacloud/chat-model", Chain: []string{"mock-main"}, Priority: 100, Enabled: true,
	}); err != nil {
		t.Fatalf("put rule: %v", err)
	}
	// Stored fallback list: MODEL IDs (the wire shape pinned by the admin
	// API contract; here seeded directly — the admin-write path is covered
	// by the reload test below).
	if err := st.PutVisionFallback(ctx, []string{"ollamacloud/fallback-vision"}); err != nil {
		t.Fatalf("put vision fallback: %v", err)
	}

	main := mockprovider.New(t)
	fallback := mockprovider.New(t)
	srv, err := New(&cfg, st, nil,
		WithProvider(main.Provider("mock-main", providerAllCaps())),
		WithProvider(fallback.Provider("mock-fb", providerAllCaps())),
		WithMasterKey(wiringMasterKey(t)),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()

	_, key, err := authCreateKey(ctx, st)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	// The routed model rejects the image, then serves the captioned retry;
	// the vision model answers with a description.
	main.Enqueue(gapRejectedScript())
	main.Enqueue(chatOKScript("chat-model", "main-answer"))
	fallback.Enqueue(chatOKScript("fallback-vision", "a terminal showing a nil-pointer stack trace"))

	status, body := capabilityFallbackChat(t, ts, key)
	if status != http.StatusOK {
		t.Fatalf("chat: status %d, body %s (the captioned retry must be served)", status, body)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("chat body: %v", err)
	}
	// The ROUTED model answered — not the vision model. That is the whole
	// design: the rescue restores the operator's routing, it does not replace it.
	if len(parsed.Choices) != 1 || parsed.Choices[0].Message.Content != "main-answer" {
		t.Fatalf("chat body: %s (want the routed model's answer)", body)
	}

	// The vision model received the image and the instruction — never the
	// conversation. The earlier turns must not appear in what it was sent.
	visionReqs := fallback.Requests()
	if len(visionReqs) != 1 {
		t.Fatalf("vision model requests = %d, want 1", len(visionReqs))
	}
	if strings.Contains(visionReqs[0].Body, "EARLIER-TURN-MARKER") ||
		strings.Contains(visionReqs[0].Body, "EARLIER-REPLY-MARKER") {
		t.Error("the conversation was forwarded to the vision model")
	}
	if !strings.Contains(visionReqs[0].Body, "image/png") {
		t.Errorf("the vision model received no image: %s", visionReqs[0].Body)
	}

	// The gap is learned into the primary model's row (async write — poll
	// briefly; learning must not be on the request path).
	deadline := time.Now().Add(3 * time.Second)
	for {
		row, err := st.GetModel(ctx, "ollamacloud/chat-model")
		if err != nil {
			t.Fatalf("get model: %v", err)
		}
		if strings.Contains(string(row.Capabilities), `"vision":false`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("learning never landed: caps = %s", row.Capabilities)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestServer_ModelTestEndpoint: the wired TestModel answers one minimal
// "Sag OK" round trip against the model's provider through the admin API —
// success carries reply + latency, an upstream failure carries the
// redacted cause, both as ok:true/false.
func TestServer_ModelTestEndpoint(t *testing.T) {
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
	if err := st.PutModel(ctx, storage.Model{
		ID: "ollamacloud/chat", ProviderID: "mock-main", Name: "chat",
	}); err != nil {
		t.Fatalf("put model: %v", err)
	}

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
	login := bootstrapAdmin(t, ts)

	// Success: the scripted reply comes back with the wire model.
	mock.Enqueue(mockprovider.NewScript().JSONResponse(http.StatusOK,
		`{"id":"c","object":"chat.completion","model":"chat","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil))
	resp := adminCall(t, ts, http.MethodPost, "/api/admin/models/test/ollamacloud/chat", "", login)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("model test: status %d, body %s", resp.StatusCode, ensureBodyClosed(t, resp))
	}
	var result struct {
		OK        bool   `json:"ok"`
		LatencyMS int64  `json:"latency_ms"`
		Reply     string `json:"reply"`
		Error     string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	_ = resp.Body.Close()
	if !result.OK || result.Reply != "OK" {
		t.Errorf("result = %+v, want ok + reply OK", result)
	}

	// The upstream request carried the row's WIRE name and the minimal ask.
	rec := mock.Requests()[0]
	var upstream struct {
		Model    string `json:"model"`
		MaxTok   int    `json:"max_completion_tokens"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(rec.Body), &upstream); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if upstream.Model != "chat" {
		t.Errorf("upstream model = %q, want the wire name", upstream.Model)
	}
	if upstream.MaxTok != 16 || upstream.Stream {
		t.Errorf("upstream budget = %d stream=%v, want max_completion_tokens 16 non-streaming", upstream.MaxTok, upstream.Stream)
	}
	if len(upstream.Messages) != 1 || upstream.Messages[0].Content != "Sag OK" {
		t.Errorf("upstream messages = %+v, want the single minimal ask", upstream.Messages)
	}

	// Failure: the cause is plain-language, ok:false.
	mock.Enqueue(mockprovider.NewScript().JSONResponse(http.StatusBadRequest,
		`{"error":{"message":"model overloaded"}}`, nil))
	resp = adminCall(t, ts, http.MethodPost, "/api/admin/models/test/ollamacloud/chat", "", login)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("failed model test: status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	_ = resp.Body.Close()
	if result.OK || result.Error == "" {
		t.Errorf("failed result = %+v, want ok:false + cause", result)
	}

	// Unknown model: ok:false with a clear cause (not a 500).
	resp = adminCall(t, ts, http.MethodPost, "/api/admin/models/test/no-such", "", login)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unknown model test: status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	_ = resp.Body.Close()
	if result.OK || result.Error != "unknown model" {
		t.Errorf("unknown result = %+v, want ok:false + unknown model", result)
	}
}

// admin API swaps the live executor entries without restart — the NEW
// model serves the next gap-rescued request.
// TestServer_FallbackWriteLiveSwapsModels: a fallback write through the
// admin API swaps the live executor entries without restart — the NEW
// model serves the next gap-rescued request.
func TestServer_FallbackWriteLiveSwapsModels(t *testing.T) {
	ctx := context.Background()
	cfg := newConfig(t)

	st, err := storage.Open(cfg.Storage)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	for _, id := range []string{"mock-main", "mock-fb1", "mock-fb2"} {
		if err := st.EnsureProvider(ctx, id, "custom", id); err != nil {
			t.Fatalf("ensure provider: %v", err)
		}
	}
	for _, m := range []storage.Model{
		{ID: "ollamacloud/chat-model", ProviderID: "mock-main", Name: "chat-model"},
		{ID: "ollamacloud/fb-one", ProviderID: "mock-fb1", Name: "fb-one"},
		{ID: "ollamacloud/fb-two", ProviderID: "mock-fb2", Name: "fb-two"},
	} {
		if err := st.PutModel(ctx, m); err != nil {
			t.Fatalf("put model %s: %v", m.ID, err)
		}
	}
	if err := st.PutRoutingRule(ctx, storage.RoutingRule{
		ID: "r1", MatchModel: "ollamacloud/chat-model", Chain: []string{"mock-main"}, Priority: 100, Enabled: true,
	}); err != nil {
		t.Fatalf("put rule: %v", err)
	}

	main := mockprovider.New(t)
	fb1 := mockprovider.New(t)
	fb2 := mockprovider.New(t)
	srv, err := New(&cfg, st, nil,
		WithProvider(main.Provider("mock-main", providerAllCaps())),
		WithProvider(fb1.Provider("mock-fb1", providerAllCaps())),
		WithProvider(fb2.Provider("mock-fb2", providerAllCaps())),
		WithMasterKey(wiringMasterKey(t)),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()
	login := bootstrapAdmin(t, ts)

	_, key, err := authCreateKey(ctx, st)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	// Admin write: fb-one describes images (live swap into the executor).
	saveFallbacks(t, ts, `["ollamacloud/fb-one"]`, login)

	main.Enqueue(gapRejectedScript())
	main.Enqueue(chatOKScript("chat-model", "main-answer"))
	fb1.Enqueue(chatOKScript("fb-one", "described by fb-one"))
	status, body := capabilityFallbackChat(t, ts, key)
	if status != http.StatusOK || !strings.Contains(body, "main-answer") {
		t.Fatalf("after write: status %d, body %s (the routed model must answer the captioned retry)", status, body)
	}
	if got := len(fb1.Requests()); got != 1 {
		t.Fatalf("fb-one requests = %d, want 1 (the written fallback was not consulted)", got)
	}

	// Admin write swaps to fb-two — the next image is described by fb-two.
	saveFallbacks(t, ts, `["ollamacloud/fb-two"]`, login)
	main.Enqueue(gapRejectedScript())
	main.Enqueue(chatOKScript("chat-model", "main-answer"))
	fb2.Enqueue(chatOKScript("fb-two", "described by fb-two"))
	status, body = capabilityFallbackChat(t, ts, key)
	if status != http.StatusOK || !strings.Contains(body, "main-answer") {
		t.Fatalf("after swap: status %d, body %s", status, body)
	}
	if got := len(fb2.Requests()); got != 1 {
		t.Fatalf("fb-two requests = %d, want 1 (live swap failed)", got)
	}
}

// visionFallbackChat issues one image-bearing chat request whose text body
// is sized to exceed the smaller fallback model's declared window.
func visionFallbackChat(t *testing.T, ts *httptest.Server, key string, textChars int) (int, string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"model": "ollamacloud/chat-model",
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": strings.Repeat("a", textChars)},
				{"type": "image_url", "image_url": map[string]string{"url": "data:image/png;base64,x"}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := ensureBodyClosed(t, resp)
	return resp.StatusCode, body
}

// TestServer_VisionFallbackIgnoresTheVisionModelsContextWindow is the
// regression for the reported scenario: a long agentic session that grows a
// screenshot mid-conversation, while every configured vision model is far
// smaller than the conversation.
//
// The vision model only ever receives the image and the instruction, so its
// declared window cannot disqualify it. The description comes back, and the
// model that answers is still the ROUTED one. This is the property the
// previous whole-conversation resend could not offer — that design had to
// re-send the history, so a smaller vision model was structurally unusable
// exactly when the rescue was needed.
func TestServer_VisionFallbackIgnoresTheVisionModelsContextWindow(t *testing.T) {
	ctx := context.Background()
	cfg := newConfig(t)

	st, err := storage.Open(cfg.Storage)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	for _, id := range []string{"mock-main", "mock-small", "mock-big"} {
		if err := st.EnsureProvider(ctx, id, "custom", id); err != nil {
			t.Fatalf("ensure provider: %v", err)
		}
	}
	for _, m := range []storage.Model{
		{ID: "ollamacloud/chat-model", ProviderID: "mock-main", Name: "chat-model"},
		// small-vision declares a 128000-token window — far below the
		// conversation below; big-vision a 1M one.
		{ID: "ollamacloud/small-vision", ProviderID: "mock-small", Name: "small-vision",
			Compat: json.RawMessage(`{"context_window":128000}`)},
		{ID: "ollamacloud/big-vision", ProviderID: "mock-big", Name: "big-vision",
			Compat: json.RawMessage(`{"context_window":1000000}`)},
	} {
		if err := st.PutModel(ctx, m); err != nil {
			t.Fatalf("put model %s: %v", m.ID, err)
		}
	}
	if err := st.PutRoutingRule(ctx, storage.RoutingRule{
		ID: "r1", MatchModel: "ollamacloud/chat-model", Chain: []string{"mock-main"}, Priority: 100, Enabled: true,
	}); err != nil {
		t.Fatalf("put rule: %v", err)
	}
	if err := st.PutVisionFallback(ctx, []string{"ollamacloud/small-vision", "ollamacloud/big-vision"}); err != nil {
		t.Fatalf("put vision fallback: %v", err)
	}

	main := mockprovider.New(t)
	small := mockprovider.New(t)
	big := mockprovider.New(t)
	srv, err := New(&cfg, st, nil,
		WithProvider(main.Provider("mock-main", providerAllCaps())),
		WithProvider(small.Provider("mock-small", providerAllCaps())),
		WithProvider(big.Provider("mock-big", providerAllCaps())),
		WithMasterKey(wiringMasterKey(t)),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	_, key, err := authCreateKey(ctx, st)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	main.Enqueue(gapRejectedScript())
	main.Enqueue(chatOKScript("chat-model", "main-answer"))
	small.Enqueue(chatOKScript("small-vision", "a screenshot of a failing test run"))

	// 600000 chars ≈ 150000 tokens — larger than small-vision's declared
	// window, and it must not matter: the vision model never sees the history.
	status, body := visionFallbackChat(t, ts, key, 600000)
	if status != http.StatusOK || !strings.Contains(body, "main-answer") {
		t.Fatalf("long-session vision fallback: status %d, body %s (the routed model must answer the captioned retry)", status, body)
	}
	if got := len(small.Requests()); got != 1 {
		t.Errorf("the first vision model received %d request(s), want 1 — its declared window must not disqualify it", got)
	}
	if got := len(big.Requests()); got != 0 {
		t.Errorf("the second vision model received %d request(s), want 0 — the first one answered", got)
	}
	// And the vision model was not handed the conversation it cannot hold.
	if got := small.Requests(); len(got) == 1 && strings.Contains(got[0].Body, strings.Repeat("a", 1000)) {
		t.Error("the conversation body was forwarded to the vision model")
	}
}
