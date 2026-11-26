package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/tubeof/proxyllm/internal/storage"
)

// TestModelCatalog_Shape validates GET /v1/model_catalog against the pinned
// Codex catalog schema: top-level "models" (never "data"), slug/display_name,
// and supported_reasoning_levels present on every entry.
func TestModelCatalog_Shape(t *testing.T) {
	e := newEnv(t)
	e.seedModel(t, "glm-5.3", json.RawMessage(`{"context_window":200000}`))
	if err := e.store.PutModel(t.Context(), storage.Model{
		ID: "vision-model", ProviderID: "mock-main", Name: "Vision",
		Capabilities: json.RawMessage(`{"vision":true}`),
	}); err != nil {
		t.Fatal(err)
	}

	status, body := e.get(t, "/v1/model_catalog")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body %s", status, body)
	}

	compiler := jsonschema.NewCompiler()
	sch, err := compiler.Compile("../../e2e/testdata/schemas/codex_model_catalog.json")
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	var doc any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if err := sch.Validate(doc); err != nil {
		t.Fatalf("catalog violates the Codex schema: %v\n%s", err, body)
	}

	var catalog struct {
		Models []struct {
			Slug                     string            `json:"slug"`
			DisplayName              string            `json:"display_name"`
			ContextWindow            int               `json:"context_window"`
			SupportedReasoningLevels []json.RawMessage `json:"supported_reasoning_levels"`
			InputModalities          []string          `json:"input_modalities"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(body), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 2 {
		t.Fatalf("models = %d, want 2", len(catalog.Models))
	}
	bySlug := map[string]int{}
	for i, m := range catalog.Models {
		bySlug[m.Slug] = i
		if m.SupportedReasoningLevels == nil {
			t.Errorf("%s: supported_reasoning_levels missing (codex 0.153 rejects it)", m.Slug)
		}
	}
	i, ok := bySlug["glm-5.3"]
	if !ok {
		t.Fatalf("glm-5.3 missing: %s", body)
	}
	if catalog.Models[i].ContextWindow != 200000 {
		t.Errorf("glm-5.3 context_window = %d, want 200000", catalog.Models[i].ContextWindow)
	}
	if !strings.Contains(body, `"models"`) || strings.Contains(body, `"object":"list"`) {
		t.Errorf("catalog is not the Codex shape: %s", body)
	}
	vi, ok := bySlug["vision-model"]
	if !ok {
		t.Fatal("vision-model missing")
	}
	if len(catalog.Models[vi].InputModalities) != 2 {
		t.Errorf("vision-model input_modalities = %v, want text+image", catalog.Models[vi].InputModalities)
	}
}

// Without a client key the catalog is closed like every other client route.
func TestModelCatalog_RequiresClientKey(t *testing.T) {
	e := newEnv(t)
	req, err := http.NewRequest(http.MethodGet, e.srv.URL+"/v1/model_catalog", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}
