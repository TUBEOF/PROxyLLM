package ollamacloud

// Model discovery contract tests (ADR-010 model sync). Since 2026-09-22
// the cloud provider rides the OpenAI-compatible surface: GET /v1/models
// (docs.ollama.com/api/openai-compatibility, accessed 2026-09-22),
// {data:[{id,...}]} — the entry "id" is the wire model identifier (the
// /v1/chat/completions "model" value).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tubeof/proxyllm/internal/provider"
)

func TestListModels_Contract(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("request = %s %s, want GET /v1/models", r.Method, r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer key" {
			t.Errorf("Authorization = %q, want Bearer key", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[` +
			`{"id":"gpt-oss:120b","object":"model","owned_by":"library"},` +
			`{"id":"qwen3-coder:480b-cloud","object":"model","owned_by":"library"}]}`))
	}))
	defer upstream.Close()

	p := New("acc-1", upstream.URL, "key", provider.Capabilities{})
	got, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 2 || got[0].Name != "gpt-oss:120b" || got[1].Name != "qwen3-coder:480b-cloud" {
		t.Errorf("ListModels = %+v, want the fixture's models in order", got)
	}
}

func TestListModels_ErrorSurfaces(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer upstream.Close()

	p := New("acc-1", upstream.URL, "key", provider.Capabilities{})
	if _, err := p.ListModels(context.Background()); err == nil {
		t.Error("non-2xx must surface as an error (the syncer records the skip)")
	}
}
