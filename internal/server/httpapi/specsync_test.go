package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// G2 (ADR-027): the OpenAI chat response the proxy emits is validated against
// the PINNED official OpenAI OpenAPI document, not only against this repo's
// hand-maintained schemas. The extract under e2e/testdata/schemas carries its
// source commit in .meta.json; it is generated mechanically from that commit.
func TestOpenAIChatResponse_MatchesOfficialSchema(t *testing.T) {
	e := newEnv(t)
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))
	status, body := e.post(t, "/v1/chat/completions", string(golden(t, "openai/chat-request.json")))
	if status != http.StatusOK {
		t.Fatalf("status = %d, body %s", status, body)
	}

	compiler := jsonschema.NewCompiler()
	sch, err := compiler.Compile("../../e2e/testdata/schemas/openai_chat_completion.official.json")
	if err != nil {
		t.Fatalf("compile official schema: %v", err)
	}
	var doc any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if err := sch.Validate(doc); err != nil {
		t.Fatalf("response violates the official OpenAI schema: %v\n%s", err, body)
	}
}
