package httpx

// The response-header hook: a provider that reads usage signals off live
// traffic (the Codex backend's rate-limit headers) must see the response
// headers on every chat round trip, streaming and non-streaming, without
// the transport growing per-provider special cases.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tubeof/proxyllm/internal/translate/ir"
)

func recordingHook(seen *http.Header) func(http.Header) {
	return func(h http.Header) { *seen = h.Clone() }
}

// A non-streaming chat dispatch hands the response headers to the hook.
func TestComplete_ResponseHeaderHookSeesHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-codex-primary-used-percent", "12.5")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	var seen http.Header
	c := &Client{BaseURL: srv.URL, ChatPath: "/", ErrorShape: ErrorOpenAI,
		HTTP: srv.Client(), ResponseHeaderHook: recordingHook(&seen)}
	_, err := c.Complete(context.Background(), ir.Request{Model: "m"},
		func(ir.Request) ([]byte, error) { return []byte(`{}`), nil },
		func([]byte) (ir.Response, error) { return ir.Response{}, nil })
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if seen.Get("x-codex-primary-used-percent") != "12.5" {
		t.Errorf("hook must see the response headers, got %+v", seen)
	}
}

// A streaming chat dispatch hands the response headers to the hook once,
// before the first event is pumped.
func TestStream_ResponseHeaderHookSeesHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-codex-secondary-used-percent", "40")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	var seen http.Header
	c := &Client{BaseURL: srv.URL, ChatPath: "/", ErrorShape: ErrorOpenAI,
		HTTP: srv.Client(), ResponseHeaderHook: recordingHook(&seen)}
	events, cancel, err := c.Stream(context.Background(), ir.Request{Model: "m"},
		func(ir.Request) ([]byte, error) { return []byte(`{}`), nil },
		newCommentDecoder)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if seen.Get("x-codex-secondary-used-percent") != "40" {
		t.Errorf("hook must see the response headers, got %+v", seen)
	}
	// Drain before returning: the pump goroutine must be gone before the
	// next test mutates the package-level watchdog timeout.
	drain(t, events)
	cancel()
}
