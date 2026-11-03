package gemini

// Provider-wire tests: the native Gemini provider must POST
// /v1beta/models/{model}:generateContent with the x-goog-api-key header
// (never a Bearer), stream :streamGenerateContent?alt=sse, page the
// /v1beta/models catalog, and count tokens via :countTokens with the full
// generateContentRequest (contents[] and generateContentRequest are mutually
// exclusive). Expectations derive from the golden fixtures in
// testdata/golden/gemini/ (official API reference + discovery doc,
// accessed 2026-11-03).

import (
	"context"
	"encoding/json"
	"errors"
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
		Model:     "gemini-2.5-flash",
		MaxTokens: 64,
		Messages: []ir.Message{{
			Role:  ir.RoleUser,
			Parts: []ir.Part{ir.TextPart{Text: "hi"}},
		}},
	}
}

func TestComplete_SendsGenerateContentWire(t *testing.T) {
	golden, err := os.ReadFile("../../../testdata/golden/gemini/generate-response.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var gotPath string
	var gotKey, gotAuth string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-goog-api-key")
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(golden)
	}))
	t.Cleanup(srv.Close)

	p := New("acc-gemini", srv.URL, "test-auth-value", provider.Capabilities{Streaming: true, Tools: true})
	resp, err := p.Complete(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotPath != "/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Errorf("path = %q", gotPath)
	}
	if gotKey != "test-auth-value" {
		t.Errorf("x-goog-api-key = %q", gotKey)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want none (the Gemini auth header is x-goog-api-key, never Bearer)", gotAuth)
	}
	// The chat body carries the body form, not a model field (the path names it).
	if _, ok := body["contents"]; !ok {
		t.Errorf("contents missing: %v", body)
	}
	if _, ok := body["model"]; ok {
		t.Errorf("body must not carry a model field: %v", body)
	}
	if resp.Model != "gemini-2.5-flash" || resp.StopReason != ir.StopToolUse {
		t.Errorf("decoded response = %+v / %q", resp, resp.StopReason)
	}
}

func TestStream_SendsSSEWire(t *testing.T) {
	golden, err := os.ReadFile("../../../testdata/golden/gemini/stream-generate.sse")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var gotURL string
	var gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(golden)
	}))
	t.Cleanup(srv.Close)

	p := New("acc-gemini", srv.URL, "", provider.Capabilities{Streaming: true})
	ch, cancel, err := p.Stream(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer cancel()
	if gotURL != "/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse" {
		t.Errorf("url = %q", gotURL)
	}
	if gotAccept != "text/event-stream" {
		t.Errorf("Accept = %q", gotAccept)
	}

	var sawStart, sawTerminal bool
	var text strings.Builder
	for ev := range ch {
		switch ev.Type {
		case ir.Start:
			sawStart = true
		case ir.Delta:
			if ev.Delta.Kind == ir.DeltaText {
				text.WriteString(ev.Delta.Text)
			}
		case ir.MessageDelta:
			sawTerminal = true
			if ev.MessageDelta.StopReason != ir.StopToolUse {
				t.Errorf("stop reason = %q, want tool_use", ev.MessageDelta.StopReason)
			}
		case ir.Error:
			t.Fatalf("stream error: %v", ev.Error)
		}
	}
	if !sawStart || !sawTerminal {
		t.Fatalf("events: start=%v terminal=%v", sawStart, sawTerminal)
	}
	if text.String() != "Hello world" {
		t.Errorf("streamed text = %q", text.String())
	}
}

func TestStream_ErrorChunksTerminate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mid-stream failure shape: a chunk carrying an error object.
		_, _ = w.Write([]byte("data: {\"error\":{\"code\":500,\"message\":\"boom\",\"status\":\"INTERNAL\"}}\n\n"))
	}))
	t.Cleanup(srv.Close)

	p := New("acc-gemini", srv.URL, "", provider.Capabilities{Streaming: true})
	ch, cancel, err := p.Stream(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer cancel()
	saw := false
	for ev := range ch {
		if ev.Type == ir.Error {
			saw = true
			if ev.Error.Message != "boom" {
				t.Errorf("error message = %q", ev.Error.Message)
			}
		}
	}
	if !saw {
		t.Error("mid-stream error chunk did not surface an Error event")
	}
}

// ListModels pages the catalog and maps the Model resource onto ModelInfo:
// name minus the models/ prefix, thinking from the resource flag, context
// length from inputTokenLimit, embedding rows filtered out.
func TestListModels_PagesAndMaps(t *testing.T) {
	golden, err := os.ReadFile("../../../testdata/golden/gemini/models-list.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var gotPageSize, gotPageToken string
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotPageSize = r.URL.Query().Get("pageSize")
		gotPageToken = r.URL.Query().Get("pageToken")
		if gotPageToken == "" {
			_, _ = w.Write(golden) // page 1: two chat models + one embedding, nextPageToken set
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-3-pro","thinking":true,"inputTokenLimit":1048576,"supportedGenerationMethods":["generateContent"]}]}`))
	}))
	t.Cleanup(srv.Close)

	p := New("acc-gemini", srv.URL, "test-auth-value", provider.Capabilities{Streaming: true})
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (paged)", calls)
	}
	if gotPageSize != "1000" {
		t.Errorf("pageSize = %q, want 1000", gotPageSize)
	}
	if gotPageToken != "cGFnZS0y" {
		t.Errorf("pageToken = %q, want the fixture's nextPageToken", gotPageToken)
	}
	if len(models) != 3 {
		t.Fatalf("models = %d (%+v), want 3", len(models), models)
	}
	// Names lose the models/ prefix.
	names := map[string]provider.ModelInfo{}
	for _, m := range models {
		names[m.Name] = m
	}
	if m, ok := names["gemini-2.5-flash"]; !ok || !m.Thinking || m.ContextLength != 1048576 || !m.Tools || !m.Vision {
		t.Errorf("gemini-2.5-flash = %+v", m)
	}
	if m, ok := names["gemini-2.0-flash"]; !ok || m.Thinking {
		t.Errorf("gemini-2.0-flash = %+v", m)
	}
	if _, ok := names["text-embedding-004"]; ok {
		t.Error("embedding model must not become a chat model row")
	}
	if _, ok := names["gemini-3-pro"]; !ok {
		t.Error("page-2 model missing")
	}
}

func TestCountTokens_SendsGenerateContentRequest(t *testing.T) {
	var gotPath string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("request body: %v", err)
		}
		_, _ = w.Write([]byte(`{"totalTokens": 342}`))
	}))
	t.Cleanup(srv.Close)

	p := New("acc-gemini", srv.URL, "", provider.Capabilities{})
	total, err := p.CountTokens(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if gotPath != "/v1beta/models/gemini-2.5-flash:countTokens" {
		t.Errorf("path = %q", gotPath)
	}
	// contents[] and generateContentRequest are mutually exclusive; the
	// provider sends the full request.
	if _, ok := body["generateContentRequest"]; !ok {
		t.Errorf("generateContentRequest missing: %v", body)
	}
	if len(body) != 1 {
		t.Errorf("countTokens body keys = %v, want exactly [generateContentRequest]", body)
	}
	if total != 342 {
		t.Errorf("totalTokens = %d, want 342", total)
	}
}

func TestErrorShape_MapsStatusIntoType(t *testing.T) {
	golden, err := os.ReadFile("../../../testdata/golden/gemini/error-body.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write(golden)
	}))
	t.Cleanup(srv.Close)

	p := New("acc-gemini", srv.URL, "", provider.Capabilities{})
	_, err = p.Complete(context.Background(), testRequest())
	if err == nil {
		t.Fatal("Complete succeeded on a 429")
	}
	var up *provider.UpstreamError
	if !errors.As(err, &up) {
		t.Fatalf("error = %T (%v), want *provider.UpstreamError", err, err)
	}
	if up.HTTPStatus != http.StatusTooManyRequests {
		t.Errorf("HTTPStatus = %d", up.HTTPStatus)
	}
	if up.Type != "RESOURCE_EXHAUSTED" {
		t.Errorf("Type = %q, want the vendor status string", up.Type)
	}
	if !strings.Contains(up.Message, "Resource exhausted") {
		t.Errorf("Message = %q", up.Message)
	}
}

func TestDefaultBaseURL(t *testing.T) {
	p := New("acc-gemini", "", "", provider.Capabilities{})
	if p.client.BaseURL != DefaultBaseURL {
		t.Errorf("base URL = %q, want %q", p.client.BaseURL, DefaultBaseURL)
	}
}

func TestPreviewRequest_IsTheChatBody(t *testing.T) {
	p := New("acc-gemini", "", "k", provider.Capabilities{})
	raw, err := p.PreviewRequest(testRequest())
	if err != nil {
		t.Fatalf("PreviewRequest: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("preview body: %v", err)
	}
	if _, ok := body["contents"]; !ok {
		t.Errorf("preview body = %v", body)
	}
	if _, ok := body["model"]; ok {
		t.Errorf("preview body must not carry a model field: %v", body)
	}
}

func TestType(t *testing.T) {
	p := New("acc-gemini", "", "", provider.Capabilities{})
	if p.Type() != "gemini" {
		t.Errorf("Type() = %q, want gemini", p.Type())
	}
}
