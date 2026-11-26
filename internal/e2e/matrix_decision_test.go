package e2e

// Decision-surface matrix cell (decision__systemone): POST /v1/systemone
// drives the fully assembled proxy end to end, driven with the golden decision
// fixture the way a client drives it. The upstream account is ollama-cloud,
// which speaks the System One wire, and MockServer answers its POST with the
// golden decision response fixture verbatim, so a green cell proves surface →
// IR → upstream wire and back. No LLM provider type exists for decision
// surfaces; the plain httpResponse expectation is the simulator's documented
// way to replay a fixed wire body.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/translate/systemone"
)

// postDecision POSTs one client decision body to the proxy with the matrix
// key, returning status and body.
func postDecision(t *testing.T, e *matrixEnv, path string, body []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}

// TestMatrix_Decision_SystemOne drives POST /v1/systemone end to end: the
// golden systemone request (model field swapped to the routing identity)
// reaches an ollama-cloud decision account, whose /v1/systemone POST MockServer
// answers with the golden response verbatim; the client sees the answers in
// the same shape, and the upstream request carries the resolved wire model.
func TestMatrix_Decision_SystemOne(t *testing.T) {
	ctx := context.Background()
	up := upstream{ID: "decision-sim", Type: "ollama-cloud", Caps: provider.Capabilities{Decision: true}}
	e := newMatrixEnv(t, up)
	// The row maps the routing identity to the fixtures' own wire name.
	if err := e.store.PutModel(ctx, storage.Model{ID: up.ID, ProviderID: up.ID, Name: "clef:cloud"}); err != nil {
		t.Fatalf("seed model row: %v", err)
	}
	e.ms.Upsert(t, map[string]any{
		"httpRequest":  map[string]any{"method": "POST", "path": "/v1/systemone"},
		"httpResponse": map[string]any{"statusCode": 200, "body": string(golden(t, "systemone/response.json"))},
	})

	// The client body is the golden request verbatim, with only the model
	// field renamed to the routing identity the proxy resolves back.
	req, err := systemone.Codec{}.DecodeRequest(golden(t, "systemone/request.json"))
	if err != nil {
		t.Fatalf("decode golden request: %v", err)
	}
	req.Model = up.ID
	body, err := systemone.Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode golden request: %v", err)
	}

	status, answer := postDecision(t, e, "/v1/systemone", body)
	if status != http.StatusOK {
		t.Fatalf("systemone: status %d, body %s", status, answer)
	}
	// The documented answer shape, straight from the golden fixture.
	for _, want := range []string{`"choice":"bug"`, `"noul":0.92`, `"input_tokens":174`, `"model":"decision-sim"`} {
		if !strings.Contains(answer, want) {
			t.Errorf("systemone answer missing %s: %s", want, answer)
		}
	}

	// The upstream request carries the wire model and the fixture's typed
	// questions, translated through the IR from the surface's shape.
	var seen map[string]any
	for _, r := range e.ms.Requests(t) {
		if r.Path == "/v1/systemone" {
			seen = upstreamJSON(t, r)
		}
	}
	if seen == nil {
		t.Fatal("MockServer received no POST /v1/systemone")
	}
	if seen["model"] != "clef:cloud" {
		t.Errorf("upstream model = %v, want the row's wire name clef:cloud", seen["model"])
	}
	if seen["state"] != "Our checkout has returned 500 errors since 9am." {
		t.Errorf("upstream state = %v, want the fixture's evidence text", seen["state"])
	}
	if qs, _ := seen["questions"].(map[string]any); len(qs) != 3 {
		t.Errorf("upstream questions = %v, want the fixture's 3 typed questions", seen["questions"])
	}
}

// TestMatrix_Decision_OpenAIDecisions drives POST /v1/decisions end to end:
// the golden Decisions API request arrives as the client, the
// openai-decisions account dispatches to its /v1/decisions endpoint (the
// same wire, answered by the golden response), and the answers array rides
// back with the numeric score values intact.
