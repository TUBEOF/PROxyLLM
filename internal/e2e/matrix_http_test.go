package e2e

import (
	"context"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	openaiclient "github.com/openai/openai-go/v3"
)

// G5 (ADR-025 test hardening): the streaming path through real HTTP servers.
// First-frame latency proves the proxy relays the first upstream frame instead
// of buffering the answer; the disconnect cell proves a client departure
// releases its goroutines instead of leaking them.

func TestMatrix_FirstFrameLatency(t *testing.T) {
	up := upstream{ID: "first-frame-up", Type: "custom-openai"}
	e := newMatrixEnv(t, up)

	completion := chatCompletion("first frame answer", true)
	completion["streamingPhysics"] = map[string]any{
		"timeToFirstToken": map[string]any{"timeUnit": "MILLISECONDS", "value": 100},
	}
	e.ms.Upsert(t, once(expectation("OPENAI", up.ID, "/v1/chat/completions", completion)))

	start := time.Now()
	stream := e.openai.Chat.Completions.NewStreaming(context.Background(), openaiclient.ChatCompletionNewParams{
		Model:    up.ID,
		Messages: []openaiclient.ChatCompletionMessageParamUnion{openaiclient.UserMessage("hi")},
	})
	defer func() { _ = stream.Close() }()
	got := false
	for stream.Next() {
		if len(stream.Current().Choices) > 0 {
			got = true
			break
		}
	}
	elapsed := time.Since(start)
	if !got {
		t.Fatalf("no first frame arrived: %v", stream.Err())
	}
	// The upstream delays its first token by 100 ms; 250 ms leaves ample room
	// for CI runner noise while still failing a proxy that buffers the whole
	// answer (which would take the stream's full duration).
	if elapsed > 250*time.Millisecond {
		t.Fatalf("first frame after %s, budget 250 ms (the upstream's first token is 100 ms)", elapsed)
	}
}

func TestMatrix_DisconnectNoGoroutineLeak(t *testing.T) {
	up := upstream{ID: "disconnect-up", Type: "custom-openai"}
	e := newMatrixEnv(t, up)

	// Warm the stack and settle, so the baseline includes SQLite and the
	// transport pools rather than first-request setup.
	warm := chatCompletion("warm", false)
	e.ms.Upsert(t, once(expectation("OPENAI", up.ID, "/v1/chat/completions", warm)))
	if _, err := e.openai.Chat.Completions.New(context.Background(), openaiclient.ChatCompletionNewParams{
		Model:    up.ID,
		Messages: []openaiclient.ChatCompletionMessageParamUnion{openaiclient.UserMessage("hi")},
	}); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	completion := chatCompletion("one two three four five six seven eight nine ten", true)
	completion["streamingPhysics"] = map[string]any{
		"tokensPerSecond":  5,
		"timeToFirstToken": map[string]any{"timeUnit": "MILLISECONDS", "value": 50},
	}
	e.ms.Upsert(t, once(expectation("OPENAI", up.ID, "/v1/chat/completions", completion)))

	ctx, cancel := context.WithCancel(context.Background())
	stream := e.openai.Chat.Completions.NewStreaming(ctx, openaiclient.ChatCompletionNewParams{
		Model:    up.ID,
		Messages: []openaiclient.ChatCompletionMessageParamUnion{openaiclient.UserMessage("hi")},
	})
	saw := false
	for stream.Next() {
		if len(stream.Current().Choices) > 0 {
			saw = true
			break
		}
	}
	if !saw {
		t.Fatalf("no frame before the disconnect: %v", stream.Err())
	}
	cancel()
	_ = stream.Close()

	// Eventual, not instantaneous: the transport, the handler and the log
	// writer each wind down on their own schedule.
	deadline := time.Now().Add(5 * time.Second)
	leaked := 0
	for time.Now().Before(deadline) {
		runtime.GC()
		now := runtime.NumGoroutine()
		if now <= baseline+3 {
			return
		}
		leaked = now - baseline
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutines after the disconnect: baseline %d, now %d (+%d) — a stream leaked", baseline, baseline+leaked, leaked)
}

// The header cell (G4): the client's credential is consumed at the boundary,
// allow-listed headers reach the upstream, and hop-by-hop headers never do.
// Assertions read the upstream request MockServer recorded.
func TestMatrix_HeaderPreservation(t *testing.T) {
	up := upstream{ID: "headers-up", Type: "custom-openai"}
	e := newMatrixEnv(t, up)
	e.ms.Upsert(t, once(expectation("OPENAI", up.ID, "/v1/chat/completions", chatCompletion("headers ok", false))))

	body := `{"model":"` + up.ID + `","messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, e.srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	// The matrix key authenticates the client to the proxy; the upstream must
	// never see it (the provider supplies its own credential).
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("x-api-key", e.key)
	req.Header.Set("Connection", "keep-alive, X-Test-Hop")
	req.Header.Set("X-Test-Hop", "drop-me")
	req.Header.Set("TE", "trailers")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("x-stainless-lang", "go")
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var seen bool
	for _, r := range e.ms.Requests(t) {
		if r.Path != "/v1/chat/completions" {
			continue
		}
		seen = true
		if got := strings.Join(recordedHeader(r, "Authorization"), ","); strings.Contains(got, e.key) {
			t.Errorf("client credential reached the upstream: %q", got)
		}
		if got := recordedHeader(r, "X-Api-Key"); len(got) > 0 {
			t.Errorf("client x-api-key reached the upstream: %v", got)
		}
		if got := recordedHeader(r, "X-Stainless-Lang"); len(got) == 0 || got[0] != "go" {
			t.Errorf("allow-listed x-stainless-lang = %v, want it forwarded", got)
		}
		for _, hop := range []string{"X-Test-Hop", "Te", "Upgrade"} {
			if got := recordedHeader(r, hop); len(got) > 0 {
				t.Errorf("hop-by-hop header %s reached the upstream: %v", hop, got)
			}
		}
		if got := strings.Join(recordedHeader(r, "Connection"), ","); strings.Contains(got, "X-Test-Hop") {
			t.Errorf("Connection named header reached the upstream: %q", got)
		}
	}
	if !seen {
		t.Fatal("MockServer recorded no upstream request")
	}
}
