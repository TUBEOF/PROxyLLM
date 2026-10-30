package openairesponses

// Provider-wire tests: the generic Responses provider must POST
// /v1/responses with Bearer auth, carry store:false, decode the golden
// response, stream the golden event set, and discover models via /v1/models.

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

func testRequest() ir.Request {
	return ir.Request{
		Model:     "gpt-5",
		MaxTokens: 64,
		Messages: []ir.Message{{
			Role:  ir.RoleUser,
			Parts: []ir.Part{ir.TextPart{Text: "hi"}},
		}},
	}
}

func TestComplete_SendsResponsesWire(t *testing.T) {
	golden, err := os.ReadFile("../../../testdata/golden/openai/responses-response.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var gotAuth, gotPath string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(golden)
	}))
	t.Cleanup(srv.Close)

	p := New("acc-responses", srv.URL, "test-key", provider.Capabilities{Streaming: true})
	resp, err := p.Complete(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotPath != "/v1/responses" {
		t.Errorf("path = %q, want /v1/responses", gotPath)
	}
	if store, ok := body["store"]; !ok || store != false {
		t.Errorf("store = %v, want explicit false", store)
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
	golden, err := os.ReadFile("../../../testdata/golden/openai/responses-stream.sse")
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

	p := New("acc-responses", srv.URL, "test-key", provider.Capabilities{Streaming: true})
	ch, cancel, err := p.Stream(context.Background(), testRequest())
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
	if text.Len() == 0 {
		t.Error("stream relayed no text")
	}
}

func TestListModels_UsesModelsAPI(t *testing.T) {
	golden, err := os.ReadFile("../../../testdata/golden/openai/models.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(golden)
	}))
	t.Cleanup(srv.Close)

	p := New("acc-responses", srv.URL, "test-key", provider.Capabilities{Streaming: true})
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if gotPath != "/v1/models" {
		t.Errorf("path = %q, want /v1/models", gotPath)
	}
	if len(models) == 0 {
		t.Fatal("no models decoded")
	}
}

func TestPreviewRequest_EncodesResponsesBody(t *testing.T) {
	p := New("acc-responses", "https://api.openai.com", "test-key", provider.Capabilities{})
	raw, err := p.PreviewRequest(testRequest())
	if err != nil {
		t.Fatalf("PreviewRequest: %v", err)
	}
	var body struct {
		Model string `json:"model"`
		Store *bool  `json:"store"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("preview body: %v", err)
	}
	if body.Model != "gpt-5" || body.Store == nil || *body.Store {
		t.Errorf("preview = %+v", body)
	}
}
