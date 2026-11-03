package e2e

// Matrix extras: the POST /v1/responses surface's previous_response_id
// chaining (the fix is live from PR #79). The proxy keeps each answered turn
// in a bounded per-client-key history, so a client that chains by an earlier
// response id makes the next upstream request carry the restored first turn;
// an id the proxy does not hold is the documented 404, dispatched nowhere.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	openaiclient "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

// once pins an expectation to exactly one matching request (MockServer
// times semantics), so a later round falls through to the round's own
// expectation no matter which order MockServer evaluates matchers in.
func once(exp map[string]any) map[string]any {
	exp["times"] = map[string]any{"remainingTimes": 1, "unlimited": false}
	return exp
}

// TestMatrix_History_PreviousResponseIDChain runs the chaining scenario the
// official Responses client makes of POST /v1/responses: turn 1 is minted an
// id; turn 2 names it and the upstream must see the restored first turn —
// input AND answer — prepended, because a tool result arriving without its
// tool call is the failure the history prevents.
func TestMatrix_History_PreviousResponseIDChain(t *testing.T) {
	ctx := context.Background()
	up := upstream{ID: "history-chain-up", Type: "custom-openai"}
	e := newMatrixEnv(t, up)

	// Turn 1: a plain user question, answered exactly once.
	e.ms.Upsert(t, once(expectation("OPENAI", up.ID, "/v1/chat/completions",
		chatCompletion("vermilion-turn-one", false))))

	first, err := e.openai.Responses.New(ctx, responses.ResponseNewParams{
		Model: up.ID,
		Input: responses.ResponseNewParamsInputUnion{OfString: openaiclient.String("history-turn-one")},
	})
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if !strings.Contains(first.OutputText(), "vermilion-turn-one") {
		t.Fatalf("turn 1 text = %q, want the minted turn's answer", first.OutputText())
	}
	if first.ID == "" {
		t.Fatal("turn 1 carries no response id to chain with")
	}

	// Turn 2: the client chains by the minted id alone.
	e.ms.Upsert(t, once(expectation("OPENAI", up.ID, "/v1/chat/completions",
		chatCompletion("vermilion-turn-two", false))))

	second, err := e.openai.Responses.New(ctx, responses.ResponseNewParams{
		Model:              up.ID,
		PreviousResponseID: openaiclient.String(first.ID),
		Input:              responses.ResponseNewParamsInputUnion{OfString: openaiclient.String("history-turn-two")},
	})
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if !strings.Contains(second.OutputText(), "vermilion-turn-two") {
		t.Fatalf("turn 2 text = %q, want the chained turn's answer", second.OutputText())
	}

	// The chained request's wire must restore the first turn in full, and the
	// proxy's own id must not ride along (an upstream that never minted it
	// would reject the request).
	chain := chainedUpstreamBody(t, e, "/v1/chat/completions", "history-turn-two")
	for _, marker := range []string{"history-turn-one", "vermilion-turn-one", "history-turn-two"} {
		if !strings.Contains(chain, marker) {
			t.Errorf("chained upstream request lost %q: %s", marker, chain)
		}
	}
	if strings.Contains(chain, "previous_response_id") {
		t.Errorf("the proxy's own response id was forwarded upstream: %s", chain)
	}

	// An id the proxy does not hold is the documented 404 — and it must not
	// dispatch anything.
	_, err = e.openai.Responses.New(ctx, responses.ResponseNewParams{
		Model:              up.ID,
		PreviousResponseID: openaiclient.String("resp_unknown-to-the-proxy"),
		Input:              responses.ResponseNewParamsInputUnion{OfString: openaiclient.String("history-turn-three")},
	})
	if err == nil {
		t.Fatal("an unknown previous_response_id was answered")
	}
	var apiErr *openaiclient.Error
	if !asOpenAIError(err, &apiErr) {
		t.Fatalf("error = %v, want the SDK's API error", err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", apiErr.StatusCode)
	}
	if apiErr.Type != "invalid_request_error" {
		t.Errorf("error type = %q, want invalid_request_error", apiErr.Type)
	}
	if !strings.Contains(apiErr.Message, "previous response with this previous_response_id is not available") {
		t.Errorf("error message = %q, want the documented not-found wording", apiErr.Message)
	}
	chatReqs := 0
	for _, r := range e.ms.Requests(t) {
		if r.Path == "/v1/chat/completions" {
			chatReqs++
		}
	}
	if chatReqs != 2 {
		t.Errorf("upstream chat requests = %d, want 2 (the 404 must not dispatch)", chatReqs)
	}
}

// chainedUpstreamBody returns the recorded upstream wire body of the one
// request that carries marker (exactly one such request must exist).
func chainedUpstreamBody(t *testing.T, e *matrixEnv, path, marker string) string {
	t.Helper()
	body, found := "", false
	for _, r := range e.ms.Requests(t) {
		if r.Path != path {
			continue
		}
		raw, err := json.Marshal(upstreamJSON(t, r))
		if err != nil {
			t.Fatalf("marshal upstream body: %v", err)
		}
		if strings.Contains(string(raw), marker) {
			if found {
				t.Fatalf("more than one upstream request carries %q", marker)
			}
			body, found = string(raw), true
		}
	}
	if !found {
		t.Fatalf("no upstream request to %s carries %q", path, marker)
	}
	return body
}
