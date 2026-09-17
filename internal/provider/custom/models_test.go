package custom

// Model discovery contract tests (ADR-010 model sync). Expectations derive
// from the OpenAI /v1/models golden fixture (testdata/golden/openai/
// models.json — developers.openai.com, accessed 2026-09-10): GET
// {baseURL}/models with Bearer auth, {object:"list",data:[{id,...}]} — the
// entry "id" is the wire model identifier.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tubeof/proxyllm/internal/provider"
	ttestutil "github.com/tubeof/proxyllm/internal/translate/testutil"
)

func TestListModels_Contract(t *testing.T) {
	// The golden fixture body — O9 shape.
	raw := string(ttestutil.LoadGolden(t, "openai/models.json"))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/models" {
			t.Errorf("request = %s %s, want GET /models", r.Method, r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer sk-test" {
			t.Errorf("Authorization = %q, want Bearer sk-test", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(raw))
	}))
	defer upstream.Close()

	p := New("acc-1", upstream.URL, "sk-test", provider.Capabilities{})
	got, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	// The fixture lists gpt-5 and gpt-5-mini in order.
	if len(got) != 2 || got[0].Name != "gpt-5" || got[1].Name != "gpt-5-mini" {
		t.Errorf("ListModels = %+v, want [gpt-5 gpt-5-mini] (fixture order)", got)
	}
}

func TestListModels_ErrorSurfaces(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key","type":"auth","code":401}}`))
	}))
	defer upstream.Close()

	p := New("acc-1", upstream.URL, "sk-test", provider.Capabilities{})
	if _, err := p.ListModels(context.Background()); err == nil {
		t.Error("non-2xx must surface as an error (the syncer records the skip)")
	}
}

func TestListModels_DuplicatesAndEmptyIDsDropped(t *testing.T) {
	body := `{"object":"list","data":[{"id":"m1"},{"id":""},{"id":"m1"},{"id":"m2"}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()

	p := New("acc-1", upstream.URL, "sk-test", provider.Capabilities{})
	got, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 2 || got[0].Name != "m1" || got[1].Name != "m2" {
		t.Errorf("ListModels = %+v, want [m1 m2]", got)
	}
}
