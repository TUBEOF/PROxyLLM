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
	ttestutil "github.com/tubeof/proxyllm/internal/translate/testutil"
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
// endpoint. The served bodies are the golden fixtures
// testdata/golden/ollama/show-response.json (glm-5.3-flash, a chat model)
// and show-response-clef.json (clef:cloud, the decision model), both
// verbatim live responses with their source URL and access date in the
// .meta.json sidecars. model_info carries heterogeneous
// values (string "general.architecture", numeric parameter counts) and
// the detected context_length key suffixed by arch. A decode type that
// assumes map[string]int64 silently probe-fails every real model — this
// fixture pins that away. Per-model errors are best-effort: a model
// whose /api/show fails still imports, with zero facts.
func TestListModels_EnrichesContextAndCapabilities(t *testing.T) {
	glmBody := ttestutil.LoadGolden(t, "ollama/show-response.json")
	clefBody := ttestutil.LoadGolden(t, "ollama/show-response-clef.json")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.3-flash"},{"id":"clef:cloud"},{"id":"ghost-model"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/show":
			var body struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode /api/show body: %v", err)
			}
			switch body.Model {
			case "ghost-model":
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"model 'ghost-model' not found"}`))
			case "clef:cloud":
				_, _ = w.Write(clefBody)
			default:
				_, _ = w.Write(glmBody)
			}
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
	if len(got) != 3 {
		t.Fatalf("ListModels = %+v, want 3 models", got)
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
	// The chat model's registry list carries no "decision": the flag stays
	// false — never inferred from the account's capability set.
	if got[0].Decision {
		t.Errorf("model 1 Decision = true; the registry does not list it")
	}
	// clef:cloud declares "decision" (and only decision+vision): the
	// per-model fact the sync persists into the row's capabilities.
	if got[1].Name != "clef:cloud" {
		t.Fatalf("model 2 name = %q", got[1].Name)
	}
	if !got[1].Decision {
		t.Errorf("model 2 Decision = false, want true (registry capabilities include \"decision\")")
	}
	if got[1].ContextLength != 65536 {
		t.Errorf("model 2 ContextLength = %d, want 65536", got[1].ContextLength)
	}
	if got[1].Thinking || got[1].Tools || !got[1].Vision {
		t.Errorf("model 2 capabilities = thinking:%v tools:%v vision:%v, want vision only",
			got[1].Thinking, got[1].Tools, got[1].Vision)
	}
	if got[2].Name != "ghost-model" {
		t.Fatalf("model 3 name = %q", got[2].Name)
	}
	// Review Focus 1: a failed /api/show probe imports with zero facts.
	if got[2].ContextLength != 0 || got[2].Thinking || got[2].Tools || got[2].Vision || got[2].Decision {
		t.Errorf("model 3 must carry zero facts after a failed probe: %+v", got[2])
	}
}
