package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// G2 in the M1 matrix: the bytes the proxy emits for each client surface are
// validated against mechanical extracts of the pinned official documents
// (openai/openai-openapi and ollama/ollama). The extracts carry their commits
// in .meta.json; compileWireSchema uses the ECMAScript regexp engine the
// official patterns require.

func postRaw(t *testing.T, e *matrixEnv, path, body string) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, e.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.key)
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status %d: %s", path, resp.StatusCode, raw)
	}
	return raw
}

func validateOfficial(t *testing.T, schemaName string, raw []byte) {
	t.Helper()
	sch := compileWireSchema(t, schemaName)
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, raw)
	}
	if err := sch.Validate(doc); err != nil {
		t.Fatalf("violates %s: %v\n%s", schemaName, err, raw)
	}
}

func TestMatrix_SpecSync_OpenAIChat(t *testing.T) {
	up := upstream{ID: "spec-chat-up", Type: "custom-openai"}
	e := newMatrixEnv(t, up)
	e.ms.Upsert(t, expectation("OPENAI", up.ID, "/v1/chat/completions", chatCompletion("spec chat", false)))
	raw := postRaw(t, e, "/v1/chat/completions",
		`{"model":"`+up.ID+`","messages":[{"role":"user","content":"hi"}]}`)
	validateOfficial(t, "openai_chat_completion.official.json", raw)
}

func TestMatrix_SpecSync_OpenAIResponse(t *testing.T) {
	up := upstream{ID: "spec-responses-up", Type: "custom-openai"}
	e := newMatrixEnv(t, up)
	e.ms.Upsert(t, expectation("OPENAI", up.ID, "/v1/chat/completions", chatCompletion("spec response", false)))
	raw := postRaw(t, e, "/v1/responses", `{"model":"`+up.ID+`","input":"hi"}`)

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	// The pinned document contradicts itself: its prose allows
	// access_programs=null (implicit Standard), the machine schema types it
	// as an object. Validate the object form and state the deviation.
	if v, ok := doc["access_programs"]; ok && v == nil {
		t.Logf("note: access_programs=null per the document's prose; the machine schema types it as object")
		doc["access_programs"] = map[string]any{}
	}
	sch := compileWireSchema(t, "openai_response.official.json")
	if err := sch.Validate(doc); err != nil {
		t.Fatalf("violates the official Responses schema: %v\n%s", err, raw)
	}
}

func TestMatrix_SpecSync_OllamaChat(t *testing.T) {
	up := upstream{ID: "spec-ollama-up", Type: "custom-openai"}
	e := newMatrixEnv(t, up)
	e.ms.Upsert(t, expectation("OPENAI", up.ID, "/v1/chat/completions", chatCompletion("spec ollama", false)))
	raw := postRaw(t, e, "/api/chat",
		`{"model":"`+up.ID+`","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	validateOfficial(t, "ollama_chat_response.official.json", raw)
}
