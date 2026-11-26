package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	openaiclient "github.com/openai/openai-go/v3"
	openaioption "github.com/openai/openai-go/v3/option"
)

// protoRecorder records the HTTP version the server negotiated, so the h2 cell
// fails loudly instead of silently testing h1 again.
type protoRecorder struct {
	next  http.RoundTripper
	mu    sync.Mutex
	proto string
}

func (p *protoRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := p.next.RoundTrip(req)
	if err == nil {
		p.mu.Lock()
		p.proto = resp.Proto
		p.mu.Unlock()
	}
	return resp, err
}

func (p *protoRecorder) negotiated() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.proto
}

// G5, h2 variant: the disconnect cell over an HTTP/2 client connection. The
// h1 cell (TestMatrix_DisconnectNoGoroutineLeak) proves the proxy releases the
// stream's goroutines; this cell proves the same over the h2 transport, where
// the server owns a per-stream state machine on top of the connection.
func TestMatrix_DisconnectNoGoroutineLeakHTTP2(t *testing.T) {
	up := upstream{ID: "disconnect-h2-up", Type: "custom-openai"}
	e := newMatrixEnv(t, up)

	// The environment's handler over TLS with ALPN h2; the test client trusts
	// the test certificate and negotiates HTTP/2.
	srv := httptest.NewUnstartedServer(e.handler)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	hc := srv.Client()
	rec := &protoRecorder{next: hc.Transport}
	hc.Transport = rec
	client := openaiclient.NewClient(
		openaioption.WithBaseURL(srv.URL+"/v1"),
		openaioption.WithAPIKey(e.key),
		openaioption.WithMaxRetries(0),
		openaioption.WithHTTPClient(hc),
	)

	// Warm the stack and settle, so the baseline includes SQLite, the h2
	// connection and the transport pools rather than first-request setup.
	warm := chatCompletion("warm", false)
	e.ms.Upsert(t, once(expectation("OPENAI", up.ID, "/v1/chat/completions", warm)))
	if _, err := client.Chat.Completions.New(context.Background(), openaiclient.ChatCompletionNewParams{
		Model:    up.ID,
		Messages: []openaiclient.ChatCompletionMessageParamUnion{openaiclient.UserMessage("hi")},
	}); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	if got := rec.negotiated(); got != "HTTP/2.0" {
		t.Fatalf("client connection negotiated %q, want HTTP/2.0", got)
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
	stream := client.Chat.Completions.NewStreaming(ctx, openaiclient.ChatCompletionNewParams{
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

	// Eventual, not instantaneous: the h2 server's stream state, the handler
	// and the log writer each wind down on their own schedule.
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
	t.Fatalf("goroutines after the h2 disconnect: baseline %d, now %d (+%d) — a stream leaked", baseline, baseline+leaked, leaked)
}
