package e2e

// Matrix extras: the client-abort cell. A streaming client that cancels
// mid-answer must leave the request logged as abandoned (499, never a 200 —
// abandonment.go), trigger no re-dispatch, and still leave the proxy working
// for the next request. The upstream stream is paced so the abort lands
// mid-answer deterministically.

import (
	"context"
	"strings"
	"testing"
	"time"

	openaiclient "github.com/openai/openai-go/v3"

	"github.com/tubeof/proxyllm/internal/storage"
)

// statusClientAborted is the request-log marker for a request the client
// abandoned: the client stopped reading and closed the connection before the
// answer was complete (abandonment.go calls it statusClientAborted).
const statusClientAborted = 499

// TestMatrix_Cancel_ClientAbort aborts a streaming chat request after the
// first content delta and pins the abandonment contract: the log row reads
// 499 (client-closed request), the upstream saw exactly one request (no
// fallback, no retry fired for a departure that says nothing about the
// provider), and the next request answers normally.
func TestMatrix_Cancel_ClientAbort(t *testing.T) {
	up := upstream{ID: "abort-cancel-up", Type: "custom-openai"}
	e := newMatrixEnv(t, up)

	// Round 1: a paced stream (tokensPerSecond = 10, first token after
	// 300ms) — the whole answer takes seconds, so canceling after the first
	// delta is guaranteed to land while the answer is still streaming.
	first := chatCompletion("round-one one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty", true)
	first["streamingPhysics"] = map[string]any{
		"tokensPerSecond": 10,
		"timeToFirstToken": map[string]any{
			"timeUnit": "MILLISECONDS", "value": 300,
		},
	}
	e.ms.Upsert(t, once(expectation("OPENAI", up.ID, "/v1/chat/completions", first)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := e.openai.Chat.Completions.NewStreaming(ctx, openaiclient.ChatCompletionNewParams{
		Model:    up.ID,
		Messages: []openaiclient.ChatCompletionMessageParamUnion{openaiclient.UserMessage("hi")},
	})
	defer func() { _ = stream.Close() }()
	var partial strings.Builder
	for stream.Next() {
		choices := stream.Current().Choices
		if len(choices) == 0 || choices[0].Delta.Content == "" {
			continue
		}
		partial.WriteString(choices[0].Delta.Content)
		// The client is gone mid-answer: the request context ends here.
		cancel()
		break
	}
	if partial.Len() == 0 {
		t.Fatal("the client canceled before any answer token arrived — not a mid-answer abort")
	}

	// The abandoned request is still logged — it really happened — and the
	// row reads 499 (client-closed), never a 200.
	bg := context.Background()
	var rows []storage.RequestLog
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := e.store.ListRequestLogs(bg, 8)
		if err != nil {
			t.Fatalf("list request logs: %v", err)
		}
		rows = rowsOf(got, up.ID)
		if len(rows) == 1 && rows[0].Status == statusClientAborted {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(rows) != 1 {
		t.Fatalf("abandoned request rows = %d, want 1", len(rows))
	}
	if rows[0].Status != statusClientAborted {
		t.Errorf("abandoned stream row status = %d, want %d (client-closed, not a served 200)", rows[0].Status, statusClientAborted)
	}

	// No next dispatch: a client departure says nothing about the provider,
	// so no fallback and no retry may fire for the abandoned stream.
	if n := countUpstreamRequests(t, e, "/v1/chat/completions"); n != 1 {
		t.Fatalf("upstream chat requests after the abort = %d, want 1", n)
	}

	// Reconnect: the next request answers normally.
	second := chatCompletion("round-two after-abort", false)
	e.ms.Upsert(t, once(expectation("OPENAI", up.ID, "/v1/chat/completions", second)))
	cc, err := e.openai.Chat.Completions.New(bg, openaiclient.ChatCompletionNewParams{
		Model:    up.ID,
		Messages: []openaiclient.ChatCompletionMessageParamUnion{openaiclient.UserMessage("hi again")},
	})
	if err != nil {
		t.Fatalf("successive request after the abort: %v", err)
	}
	if len(cc.Choices) == 0 || !strings.Contains(cc.Choices[0].Message.Content, "round-two after-abort") {
		t.Fatalf("successive request text = %+v, want the round-two answer", cc.Choices)
	}

	// Row 2 is the round-two success; the abandoned 499 row stands untouched.
	latest, err := e.store.ListRequestLogs(bg, 8)
	if err != nil {
		t.Fatalf("list request logs: %v", err)
	}
	dups := countUpstreamRequests(t, e, "/v1/chat/completions")
	if dups != 2 {
		t.Errorf("upstream chat requests after both rounds = %d, want 2", dups)
	}
	got := rowsOf(latest, up.ID)
	if len(got) != 2 {
		t.Fatalf("rows for %s = %d, want 2", up.ID, len(got))
	}
	if got[0].Status != 200 || got[1].Status != statusClientAborted {
		t.Errorf("row statuses (newest first) = %d,%d, want 200,%d", got[0].Status, got[1].Status, statusClientAborted)
	}
}

// rowsOf keeps the log rows that name model, newest first as listed.
func rowsOf(rows []storage.RequestLog, model string) []storage.RequestLog {
	var out []storage.RequestLog
	for _, r := range rows {
		if r.Model == model {
			out = append(out, r)
		}
	}
	return out
}

// countUpstreamRequests counts MockServer's recorded requests for path.
func countUpstreamRequests(t *testing.T, e *matrixEnv, path string) int {
	t.Helper()
	n := 0
	for _, r := range e.ms.Requests(t) {
		if r.Path == path {
			n++
		}
	}
	return n
}
