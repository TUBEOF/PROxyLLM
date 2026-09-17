package ollamacloud

// Model discovery contract tests (ADR-010 model sync). Expectations derive
// from the Ollama /api/tags golden fixture (testdata/golden/ollama/
// tags-response.json — docs.ollama.com, accessed 2026-09-17): GET
// {baseURL}/api/tags with Bearer auth, {models:[{name,...}]} — the entry
// "name" is the wire model identifier (the /api/chat "model" value).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tubeof/proxyllm/internal/provider"
	ttestutil "github.com/tubeof/proxyllm/internal/translate/testutil"
)

func TestListModels_Contract(t *testing.T) {
	raw := string(ttestutil.LoadGolden(t, "ollama/tags-response.json"))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/tags" {
			t.Errorf("request = %s %s, want GET /api/tags", r.Method, r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer key" {
			t.Errorf("Authorization = %q, want Bearer key", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(raw))
	}))
	defer upstream.Close()

	p := New("acc-1", upstream.URL, "key", provider.Capabilities{})
	got, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	// The fixture lists gpt-oss:120b and qwen3-coder:480b-cloud in order.
	if len(got) != 2 || got[0].Name != "gpt-oss:120b" || got[1].Name != "qwen3-coder:480b-cloud" {
		t.Errorf("ListModels = %+v, want the fixture's models in order", got)
	}
}

func TestListModels_ErrorSurfaces(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer upstream.Close()

	p := New("acc-1", upstream.URL, "key", provider.Capabilities{})
	if _, err := p.ListModels(context.Background()); err == nil {
		t.Error("non-2xx must surface as an error (the syncer records the skip)")
	}
}
