package anthropic

// Provider-wire tests: the Anthropic upstream must receive x-api-key and the
// Messages version header, POST /v1/messages, encode the IR body, decode the
// golden response, and stream the golden event set.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

func request() ir.Request {
	return ir.Request{
		Model:     "claude-sonnet-5",
		MaxTokens: 64,
		Messages: []ir.Message{{
			Role:  ir.RoleUser,
			Parts: []ir.Part{ir.TextPart{Text: "hi"}},
		}},
	}
}

func TestComplete_SendsAnthropicWire(t *testing.T) {
	golden, err := os.ReadFile("../../../testdata/golden/anthropic/message-response.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var gotKey, gotVersion, gotPath, gotAuth string
	var versionValues []string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		versionValues = r.Header.Values("anthropic-version")
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(golden)
	}))
	t.Cleanup(srv.Close)

	p := New("acc-anthropic", srv.URL, "test-key", provider.Capabilities{Streaming: true})
	ctx := provider.WithForwardedHeaders(context.Background(),
		http.Header{"anthropic-version": {"2024-06-01"}})
	resp, err := p.Complete(ctx, request())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotKey != "test-key" {
		t.Errorf("x-api-key = %q", gotKey)
	}
	if gotVersion != "2024-06-01" || len(versionValues) != 1 {
		t.Errorf("anthropic-version = %q (values %v), want the single client value", gotVersion, versionValues)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want none (x-api-key auth only)", gotAuth)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", gotPath)
	}
	if body["model"] != "claude-sonnet-5" {
		t.Errorf("upstream model = %v", body["model"])
	}
	var text string
	for _, part := range resp.Content {
		if tp, ok := part.(ir.TextPart); ok {
			text += tp.Text
		}
	}
	if text == "" {
		t.Errorf("decoded response has no text: %+v", resp.Content)
	}
}

func TestStream_RelaysGoldenEvents(t *testing.T) {
	golden, err := os.ReadFile("../../../testdata/golden/anthropic/stream-events.sse")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body.Stream {
			t.Error("upstream request did not set stream:true")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(golden)
	}))
	t.Cleanup(srv.Close)

	p := New("acc-anthropic", srv.URL, "test-key", provider.Capabilities{Streaming: true})
	ch, cancel, err := p.Stream(context.Background(), request())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer cancel()

	var sawStart, sawDelta, sawTerminal bool
	var text strings.Builder
	for ev := range ch {
		switch ev.Type {
		case ir.Start:
			sawStart = true
		case ir.Delta:
			sawDelta = true
			if ev.Delta.Kind == ir.DeltaText {
				text.WriteString(ev.Delta.Text)
			}
		case ir.MessageDelta:
			sawTerminal = true
		case ir.Error:
			t.Fatalf("stream error: %v", ev.Error)
		}
	}
	if !sawStart || !sawDelta || !sawTerminal {
		t.Fatalf("events: start=%v delta=%v terminal=%v", sawStart, sawDelta, sawTerminal)
	}
	if !strings.Contains(text.String(), "Hello") {
		t.Errorf("relayed text = %q, want the golden Hello delta", text.String())
	}
}

func TestListModels_UsesModelsAPI(t *testing.T) {
	golden, err := os.ReadFile("../../../testdata/golden/anthropic/models.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var gotKey, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(golden)
	}))
	t.Cleanup(srv.Close)

	p := New("acc-anthropic", srv.URL, "test-key", provider.Capabilities{Streaming: true})
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if gotKey != "test-key" || gotPath != "/v1/models" {
		t.Errorf("models request key=%q path=%q", gotKey, gotPath)
	}
	if len(models) == 0 {
		t.Fatal("no models decoded")
	}
	for _, m := range models {
		if m.Name == "" || !m.Tools || !m.Vision || !m.Thinking {
			t.Errorf("model = %+v, want name + tools/vision/thinking", m)
		}
	}
}

func TestPreviewRequest_EncodesMessagesBody(t *testing.T) {
	p := New("acc-anthropic", "https://api.anthropic.com", "test-key", provider.Capabilities{})
	raw, err := p.PreviewRequest(request())
	if err != nil {
		t.Fatalf("PreviewRequest: %v", err)
	}
	var body struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("preview body: %v", err)
	}
	if body.Model != "claude-sonnet-5" || body.MaxTokens != 64 {
		t.Errorf("preview = %+v", body)
	}
}
