package ollamanative

// Provider-wire tests: the native Ollama provider must POST /api/chat with
// the NDJSON wire, decode the golden response, stream the golden NDJSON
// events, and discover models via /api/tags.

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
		Model:     "llama3",
		MaxTokens: 64,
		Messages: []ir.Message{{
			Role:  ir.RoleUser,
			Parts: []ir.Part{ir.TextPart{Text: "hi"}},
		}},
	}
}

func TestComplete_SendsChatWire(t *testing.T) {
	golden, err := os.ReadFile("../../../testdata/golden/ollama/chat-response.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var gotPath string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(golden)
	}))
	t.Cleanup(srv.Close)

	p := New("acc-ollama", srv.URL, "", provider.Capabilities{Streaming: true})
	resp, err := p.Complete(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotPath != "/api/chat" {
		t.Errorf("path = %q, want /api/chat", gotPath)
	}
	if stream, ok := body["stream"]; !ok || stream != false {
		t.Errorf("stream = %v, want explicit false", stream)
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

func TestStream_RelaysGoldenNDJSON(t *testing.T) {
	golden, err := os.ReadFile("../../../testdata/golden/ollama/chat-stream.ndjson")
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
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write(golden)
	}))
	t.Cleanup(srv.Close)

	p := New("acc-ollama", srv.URL, "", provider.Capabilities{Streaming: true})
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

func TestListModels_UsesTags(t *testing.T) {
	golden, err := os.ReadFile("../../../testdata/golden/ollama/tags-response.json")
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

	p := New("acc-ollama", srv.URL, "", provider.Capabilities{Streaming: true})
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if gotPath != "/api/tags" {
		t.Errorf("path = %q, want /api/tags", gotPath)
	}
	if len(models) == 0 {
		t.Fatal("no models decoded")
	}
}

func TestDefaultBaseURL(t *testing.T) {
	p := New("acc-ollama", "", "", provider.Capabilities{})
	if p.client.BaseURL != DefaultBaseURL {
		t.Errorf("base URL = %q, want %q", p.client.BaseURL, DefaultBaseURL)
	}
}
