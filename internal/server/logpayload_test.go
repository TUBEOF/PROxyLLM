package server

// Captured request payloads (2026-10-10 Logs page): the conversation a request
// carried and the answer it produced are stored beside its log row and served
// on demand, so a log entry can be read rather than only counted.
//
// Two properties this suite pins beyond the happy path: image bytes are never
// stored (an image is recorded by media type and size — a base64 screenshot is
// megabytes that say nothing a size does not), and the capture is written off
// the request path, so the test has to wait for it rather than assume it.

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

// chatOnce runs one chat completion through the proxy surface.
func chatOnce(t *testing.T, ts *httptest.Server, key, body string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = ensureBodyClosed(t, resp)
	return resp.StatusCode
}

// logPayloadFor polls the payload endpoint until the (asynchronous) capture for
// the newest log row lands.
func logPayloadFor(t *testing.T, ts *httptest.Server, login wiringLogin) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var envelope struct {
			Logs []struct {
				ID int64 `json:"id"`
			} `json:"logs"`
		}
		resp := adminCall(t, ts, http.MethodGet, "/api/admin/logs?limit=1", "", login)
		body := ensureBodyClosed(t, resp)
		if err := json.Unmarshal([]byte(body), &envelope); err != nil || len(envelope.Logs) == 0 {
			t.Fatalf("logs page: %s", body)
		}
		id := envelope.Logs[0].ID

		resp = adminCall(t, ts, http.MethodGet,
			fmt.Sprintf("/api/admin/log-payload?owner=request&id=%d", id), "", login)
		body = ensureBodyClosed(t, resp)
		var payload map[string]any
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatalf("payload body: %s", body)
		}
		if found, _ := payload["found"].(bool); found {
			return payload
		}
		if time.Now().After(deadline) {
			t.Fatalf("capture never landed for row %d: %s", id, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestServer_LogPayload_CapturesConversationAndAnswer(t *testing.T) {
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
		ID: "ollamacloud/chat-model", ProviderID: "mock-main", Name: "chat-model",
	}); err != nil {
		t.Fatalf("put model: %v", err)
	}
	if err := st.PutRoutingRule(ctx, storage.RoutingRule{
		ID: "r1", MatchModel: "ollamacloud/chat-model", Chain: []string{"mock-main"}, Priority: 100, Enabled: true,
	}); err != nil {
		t.Fatalf("put rule: %v", err)
	}

	main := mockprovider.New(t)
	srv, err := New(&cfg, st, nil,
		WithProvider(main.Provider("mock-main", providerAllCaps())),
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

	main.Enqueue(chatOKScript("chat-model", "the answer"))
	const imagePayload = "iVBORw0KGgoAAAANSUhEUg=="
	status := chatOnce(t, ts, key, `{"model":"ollamacloud/chat-model","messages":[`+
		`{"role":"user","content":"what does this error say?"},`+
		`{"role":"user","content":[{"type":"text","text":"screenshot:"},`+
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,`+imagePayload+`"}}]}]}`)
	if status != http.StatusOK {
		t.Fatalf("chat status %d", status)
	}

	payload := logPayloadFor(t, ts, login)

	if enabled, _ := payload["capture_enabled"].(bool); !enabled {
		t.Error("capture reports itself disabled by default")
	}
	reqJSON, _ := payload["client_request"].(string)
	if !strings.Contains(reqJSON, "what does this error say?") {
		t.Errorf("the conversation was not captured: %s", reqJSON)
	}
	// The image is recorded by its shape, never by its bytes.
	if strings.Contains(reqJSON, imagePayload) {
		t.Error("the image payload was stored — a log row must not carry megabytes of base64")
	}
	if !strings.Contains(reqJSON, `"kind":"image"`) || !strings.Contains(reqJSON, "image/png") {
		t.Errorf("the image was not recorded by its media type: %s", reqJSON)
	}

	respJSON, _ := payload["client_response"].(string)
	if !strings.Contains(respJSON, "the answer") {
		t.Errorf("the answer was not captured: %s", respJSON)
	}
}

// An operator who turns capture off gets metadata-only rows, and the payload
// endpoint says so rather than leaving the drawer to guess.
func TestServer_LogPayload_CaptureOffStoresNothing(t *testing.T) {
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
		ID: "ollamacloud/chat-model", ProviderID: "mock-main", Name: "chat-model",
	}); err != nil {
		t.Fatalf("put model: %v", err)
	}
	if err := st.PutRoutingRule(ctx, storage.RoutingRule{
		ID: "r1", MatchModel: "ollamacloud/chat-model", Chain: []string{"mock-main"}, Priority: 100, Enabled: true,
	}); err != nil {
		t.Fatalf("put rule: %v", err)
	}
	if err := st.SetConfigValue(ctx, storage.RequestPayloadLoggingKey, "0"); err != nil {
		t.Fatalf("disable capture: %v", err)
	}

	main := mockprovider.New(t)
	srv, err := New(&cfg, st, nil,
		WithProvider(main.Provider("mock-main", providerAllCaps())),
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

	main.Enqueue(chatOKScript("chat-model", "the answer"))
	if status := chatOnce(t, ts, key,
		`{"model":"ollamacloud/chat-model","messages":[{"role":"user","content":"hello"}]}`); status != http.StatusOK {
		t.Fatalf("chat status %d", status)
	}

	// Give the (skipped) capture goroutine a moment; then assert nothing landed
	// rather than racing it.
	time.Sleep(100 * time.Millisecond)

	var envelope struct {
		Logs []struct {
			ID int64 `json:"id"`
		} `json:"logs"`
	}
	resp := adminCall(t, ts, http.MethodGet, "/api/admin/logs?limit=1", "", login)
	if err := json.Unmarshal([]byte(ensureBodyClosed(t, resp)), &envelope); err != nil || len(envelope.Logs) == 0 {
		t.Fatalf("logs page did not return the row")
	}
	resp = adminCall(t, ts, http.MethodGet,
		fmt.Sprintf("/api/admin/log-payload?owner=request&id=%d", envelope.Logs[0].ID), "", login)
	body := ensureBodyClosed(t, resp)
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("payload body: %s", body)
	}
	if found, _ := payload["found"].(bool); found {
		t.Errorf("a payload was stored while capture was off: %s", body)
	}
	if enabled, _ := payload["capture_enabled"].(bool); enabled {
		t.Error("the payload endpoint reports capture enabled while it is off")
	}
}
