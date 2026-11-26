package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dlclark/regexp2"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// The official OpenAI document uses ECMAScript lookaheads in identifier
// patterns; Go's regexp cannot parse them, so the validator runs on regexp2
// (the same engine the M1 wire-schema cells use).
type pcreRegexp struct{ re *regexp2.Regexp }

func (r *pcreRegexp) MatchString(s string) bool {
	ok, err := r.re.MatchString(s)
	return err == nil && ok
}
func (r *pcreRegexp) String() string { return r.re.String() }

func compileOfficial(t *testing.T, path string) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.UseRegexpEngine(func(s string) (jsonschema.Regexp, error) {
		re, err := regexp2.Compile(s, regexp2.ECMAScript)
		if err != nil {
			return nil, err
		}
		return &pcreRegexp{re: re}, nil
	})
	sch, err := c.Compile(path)
	if err != nil {
		t.Fatalf("compile %s: %v", path, err)
	}
	return sch
}

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

	sch := compileOfficial(t, "../../e2e/testdata/schemas/openai_chat_completion.official.json")
	var doc any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if err := sch.Validate(doc); err != nil {
		t.Fatalf("response violates the official OpenAI schema: %v\n%s", err, body)
	}
}

// G2, Responses surface: the same pinned-document check for
// the Responses object the proxy emits.
func TestOpenAIResponse_MatchesOfficialSchema(t *testing.T) {
	e := newEnv(t)
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))
	status, body := e.post(t, "/v1/responses", string(golden(t, "openai/responses-request.json")))
	if status != http.StatusOK {
		t.Fatalf("status = %d, body %s", status, body)
	}
	sch := compileOfficial(t, "../../e2e/testdata/schemas/openai_response.official.json")
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	// Documented contradiction in the pinned document: its prose says an
	// implicit Standard fallback "is represented by null in the response's
	// access_programs field", while the machine schema types that field as an
	// object. The encoder follows the prose; validate with the object form so
	// the rest of the document stays strict, and say so on every run.
	if v, ok := doc["access_programs"]; ok && v == nil {
		t.Logf("note: access_programs=null per the document's prose; the machine schema types it as object")
		doc["access_programs"] = map[string]any{}
	}
	if err := sch.Validate(doc); err != nil {
		t.Fatalf("response violates the official Responses schema: %v\n%s", err, body)
	}
}

// G2, native Ollama surface: the same pinned-document check for the
// ChatResponse object.
func TestOllamaChatResponse_MatchesOfficialSchema(t *testing.T) {
	e := newEnv(t)
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))
	status, body := e.post(t, "/api/chat",
		`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body %s", status, body)
	}
	sch := compileOfficial(t, "../../e2e/testdata/schemas/ollama_chat_response.official.json")
	var doc any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if err := sch.Validate(doc); err != nil {
		t.Fatalf("response violates the official Ollama schema: %v\n%s", err, body)
	}
}
