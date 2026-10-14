package ollamacloud

// Model discovery contract tests (ADR-010 model sync). Since 2026-09-22
// the cloud provider rides the OpenAI-compatible surface: GET /v1/models
// (docs.ollama.com/api/openai-compatibility, accessed 2026-09-22),
// {data:[{id,...}]} — the entry "id" is the wire model identifier (the
// /v1/chat/completions "model" value).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tubeof/proxyllm/internal/provider"
)

func TestListModels_Contract(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			if auth := r.Header.Get("Authorization"); auth != "Bearer key" {
				t.Errorf("Authorization = %q, want Bearer key", auth)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[` +
				`{"id":"gpt-oss:120b","object":"model","owned_by":"library"},` +
				`{"id":"qwen3-coder:480b-cloud","object":"model","owned_by":"library"}]}`))
		default:
			// The enrichment probes (/api/show) are best-effort; a handler
			// that fails them must still import the model list verbatim.
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"model not found"}`))
		}
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

// Context/capability detection: after the /v1/models list, ListModels
// enriches each model by
// POSTing /api/show {"model": id} — the registry's per-model facts
// endpoint. The mock body below is the VERBATIM live response for
// glm-5.3-flash (https://ollama.com/api/show, accessed 2026-10-06):
// model_info carries heterogeneous
// values (string "general.architecture", numeric parameter counts) and
// the detected context_length key suffixed by arch. A decode type that
// assumes map[string]int64 silently probe-fails every real model — this
// fixture pins that away. Per-model errors are best-effort: a model
// whose /api/show fails still imports, with zero facts.
func TestListModels_EnrichesContextAndCapabilities(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.3-flash"},{"id":"ghost-model"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/show":
			var body struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode /api/show body: %v", err)
			}
			if body.Model == "ghost-model" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"model 'ghost-model' not found"}`))
				return
			}
			// Verbatim live body (ollama.com/api/show, 2026-10-06).
			_, _ = w.Write([]byte(`{"capabilities":["completion","thinking","tools","vision"],"details":{"parent_model":"glm-5.3-flash","format":"","family":"glm5_next","families":null,"parameter_size":"321323031390","quantization_level":"FP8"},"model_info":{"general.architecture":"glm5_next","general.parameter_count":321323031390,"glm5_next.context_length":1048576,"glm5_next.embedding_length":4096},"modified_at":"2026-08-26T07:00:00-07:00","thinking":{"values":["low","high","max"],"default":"max"}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer upstream.Close()

	p := New("acc-1", upstream.URL, "key", provider.Capabilities{})
	got, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListModels = %+v, want 2 models", got)
	}
	if got[0].Name != "glm-5.3-flash" {
		t.Fatalf("model 1 name = %q", got[0].Name)
	}
	if got[0].ContextLength != 1048576 {
		t.Errorf("model 1 ContextLength = %d, want 1048576", got[0].ContextLength)
	}
	if !got[0].Thinking || !got[0].Tools || !got[0].Vision {
		t.Errorf("model 1 capabilities = thinking:%v tools:%v vision:%v, want all true", got[0].Thinking, got[0].Tools, got[0].Vision)
	}
	if got[1].Name != "ghost-model" {
		t.Fatalf("model 2 name = %q", got[1].Name)
	}
	// Review Focus 1: a failed /api/show probe imports with zero facts.
	if got[1].ContextLength != 0 || got[1].Thinking || got[1].Tools || got[1].Vision {
		t.Errorf("model 2 must carry zero facts after a failed probe: %+v", got[1])
	}
}
