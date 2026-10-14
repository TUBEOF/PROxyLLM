package httpx

// How a stream ends is part of the contract: silently, only when the upstream
// said it was done.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/translate"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// comments yields one Comment event per read and reports EOF when the body does.
// Enough to exercise the transport without dragging a protocol in: what is under
// test is when the channel closes and what arrives before it does.
func newCommentDecoder(r io.Reader) translate.StreamDecoder { return &comments{r: r} }

type comments struct{ r io.Reader }

func (c *comments) Read(context.Context) (ir.StreamEvent, error) {
	buf := make([]byte, 512)
	n, err := c.r.Read(buf)
	if err != nil {
		return ir.StreamEvent{}, err
	}
	if n == 0 {
		return ir.StreamEvent{}, io.EOF
	}
	return ir.StreamEvent{Type: ir.Comment}, nil
}

func drain(t *testing.T, events <-chan ir.StreamEvent) []ir.StreamEvent {
	t.Helper()
	var out []ir.StreamEvent
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatal("the stream never ended")
			return out
		}
	}
}

func streamClient(url string) *Client {
	return &Client{
		BaseURL: url, ChatPath: "/chat", ErrorShape: ErrorOpenAI,
		HTTP: &http.Client{},
	}
}

// A body that stops without a terminal event leaves the client holding a partial
// answer and waiting for something that is not coming. The channel must carry an
// error, not just close.
func TestStream_TruncatedStreamEndsWithAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("half a comment"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Return without a terminal marker: the handler closing the connection
		// is the upstream hanging up mid-answer.
	}))
	defer srv.Close()

	c := streamClient(srv.URL)
	events, cancel, err := c.Stream(context.Background(), ir.Request{Model: "m"},
		func(ir.Request) ([]byte, error) { return []byte(`{}`), nil }, newCommentDecoder)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer cancel()

	got := drain(t, events)
	if len(got) == 0 || got[len(got)-1].Type != ir.Error {
		t.Fatalf("events = %+v, want the last one to be an error", got)
	}
	if got[len(got)-1].Error == nil || got[len(got)-1].Error.Message == "" {
		t.Error("the terminal error carries no message; the operator would have nothing to read")
	}
}

// An upstream that accepts the request and then goes quiet must not hold the
// client forever.
func TestStream_SilentUpstreamIsCutOffByTheIdleWatchdog(t *testing.T) {
	old := stallIdleTimeout
	stallIdleTimeout = 150 * time.Millisecond
	defer func() { stallIdleTimeout = old }()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("first"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release // never another byte
	}))
	defer func() { close(release); srv.Close() }()

	c := streamClient(srv.URL)
	events, cancel, err := c.Stream(context.Background(), ir.Request{Model: "m"},
		func(ir.Request) ([]byte, error) { return []byte(`{}`), nil }, newCommentDecoder)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer cancel()

	started := time.Now()
	got := drain(t, events)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("the watchdog took %v to notice a silent upstream", elapsed)
	}
	if len(got) == 0 || got[len(got)-1].Type != ir.Error {
		t.Fatalf("events = %+v, want the stream to end with an error", got)
	}
}
