package custom_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/provider/custom"
	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/openai"
	"github.com/tubeof/proxyllm/internal/translate/testutil"
)

// Custom OpenAI-compatible provider contract tests, RED-GREEN.
// Expectations derive from the golden fixtures captured from
// the official OpenAI API reference (accessed 2026-09-10) and the
// provider interface contract — never from provider code
// behavior.
//
// Wire facts pinned here:
//   - auth is `Authorization: Bearer <key>`.
//   - chat completions POST to {base}/chat/completions.
//   - streaming is data-only SSE terminated by [DONE]; the codec emits
//     `"stream":true` for stream requests (chat-completions `stream` is
//     omitted when false — openai.go wire tag).
//   - non-2xx maps to *provider.UpstreamError carrying HTTP status,
//     body error.type/code, and Retry-After seconds.
//   - cancel aborts the upstream request; a mid-stream upstream
//     break becomes a terminal ir.Error event, never a retried dispatch.

const testAPIKey = "test-key-123"

// mustWrite is the test-handler write helper: a write failure inside a
// scripted httptest handler is always a fatal test condition.
func mustWrite(t *testing.T, w io.Writer, data []byte) {
	t.Helper()
	if _, err := w.Write(data); err != nil {
		t.Fatalf("test handler write: %v", err)
	}
}

// mustWriteString is mustWrite for string payloads (SSE frames).
func mustWriteString(t *testing.T, w io.Writer, s string) {
	t.Helper()
	if _, err := io.WriteString(w, s); err != nil {
		t.Fatalf("test handler write: %v", err)
	}
}

func newProvider(t *testing.T, handler http.HandlerFunc) (*custom.Provider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	// baseURL carries the version prefix; the provider appends
	// /chat/completions.
	return custom.New("custom-openai", srv.URL+"/v1", testAPIKey, provider.Capabilities{
		Streaming: true,
		Tools:     true,
	}), srv
}

func chatRequest() ir.Request {
	return ir.Request{
		Model:     "gpt-5",
		MaxTokens: 1024,
		Stream:    false,
		Messages: []ir.Message{{
			Role:  ir.RoleUser,
			Parts: []ir.Part{ir.TextPart{Text: "What's the weather in San Francisco?"}},
		}},
	}
}

// decodeSentBody captures and decodes the JSON body the provider POSTed.
func decodeSentBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("request body is not JSON: %v\n%s", err, raw)
	}
	return body
}

func drainEvents(t *testing.T, events <-chan ir.StreamEvent) []ir.StreamEvent {
	t.Helper()
	var out []ir.StreamEvent
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-time.After(5 * time.Second):
			t.Fatal("timed out draining stream events")
		}
	}
}

// TestComplete_RoundTrip: a 2xx chat-completions body decodes to the IR via
// the OpenAI codec; auth is Bearer; the request rides the chat
// completions path with the model stamped.
func TestComplete_RoundTrip(t *testing.T) {
	pr, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if want := "/v1/chat/completions"; r.URL.Path != want {
			t.Errorf("path = %s, want %s", r.URL.Path, want)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testAPIKey {
			t.Errorf("Authorization = %q, want Bearer %q", got, testAPIKey)
		}
		body := decodeSentBody(t, r)
		if body["model"] != "gpt-5" {
			t.Errorf("request model = %v, want gpt-5", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		mustWrite(t, w, testutil.LoadGolden(t, "openai/chat-response.json"))
	})

	resp, err := pr.Complete(context.Background(), chatRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var want ir.Response
	raw := testutil.LoadGolden(t, "openai/chat-response.json")
	want, err = openai.Codec{}.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("decode golden response: %v", err)
	}
	if !reflect.DeepEqual(resp, want) {
		t.Errorf("Complete response = %+v, want golden decode %+v", resp, want)
	}
}

// TestComplete_UpstreamError: a non-2xx response maps to
// *provider.UpstreamError with the two classification tiers — HTTP
// status plus the body error object — and Retry-After seconds.
func TestComplete_UpstreamError(t *testing.T) {
	pr, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		mustWrite(t, w, testutil.LoadGolden(t, "openai/error.json"))
	})

	_, err := pr.Complete(context.Background(), chatRequest())
	var up *provider.UpstreamError
	if !errors.As(err, &up) {
		t.Fatalf("Complete error = %v (%T), want *provider.UpstreamError", err, err)
	}
	if up.HTTPStatus != http.StatusTooManyRequests {
		t.Errorf("HTTPStatus = %d, want %d", up.HTTPStatus, http.StatusTooManyRequests)
	}
	if up.Code != "rate_limit_exceeded" || up.Type != "rate_limit_exceeded" {
		t.Errorf("Type/Code = %q/%q, want rate_limit_exceeded from the error body", up.Type, up.Code)
	}
	if up.Message == "" {
		t.Error("Message must carry the upstream error message")
	}
	if up.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %v, want 7s (Retry-After header)", up.RetryAfter)
	}
}

// TestComplete_TransportError: a failure before any response (connection
// refused) is a transport error, NOT an UpstreamError — routing classifies
// those as early-stream retryable via the zero-status path.
func TestComplete_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // connection refused from here on
	p := custom.New("dead", srv.URL+"/v1", testAPIKey, provider.Capabilities{})

	_, err := p.Complete(context.Background(), chatRequest())
	if err == nil {
		t.Fatal("Complete against a closed endpoint must fail")
	}
	var up *provider.UpstreamError
	if errors.As(err, &up) {
		t.Errorf("transport error must not be an UpstreamError, got %+v", up)
	}
}

// TestStream_Events: the SSE fixture replays to the canonical event
// sequence from the .expected.json fixture; the request body carries
// stream:true.
func TestStream_Events(t *testing.T) {
	pr, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeSentBody(t, r)
		if body["stream"] != true {
			t.Errorf("request stream = %v, want true for stream requests", body["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		mustWrite(t, w, testutil.LoadGolden(t, "openai/chat-stream.sse"))
	})

	req := chatRequest()
	req.Stream = true
	events, cancel, err := pr.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer cancel()

	got := drainEvents(t, events)
	want := testutil.LoadExpectedEvents(t, "openai/chat-stream.expected.json")
	testutil.AssertEventsEqual(t, "openai/chat-stream.sse", want, got)
}

// TestStream_MidStreamFailure: an upstream break after the first event is
// terminal — a terminal ir.Error event, then channel close. Never a
// dispatch error (mid-stream failures are never retried or swapped).
func TestStream_MidStreamFailure(t *testing.T) {
	pr, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// One complete role chunk, then the connection dies mid-frame.
		mustWriteString(t, w, "data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n")
		flusher.Flush()
		mustWriteString(t, w, "data: {\"id\":\"chatcmpl-x\"") // truncated JSON, then abrupt close
		hj := w.(http.Hijacker)
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		_ = conn.Close()
	})

	req := chatRequest()
	req.Stream = true
	events, cancel, err := pr.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream dispatch must succeed (the failure is mid-stream): %v", err)
	}
	defer cancel()

	got := drainEvents(t, events)
	if len(got) == 0 {
		t.Fatal("expected at least the start event before the failure")
	}
	last := got[len(got)-1]
	if last.Type != ir.Error {
		t.Fatalf("last event = %s, want a terminal ir.Error event", last.Type)
	}
	if last.Error == nil || last.Error.Message == "" {
		t.Error("terminal error event must carry the upstream failure message")
	}
}

// TestStream_Cancel: the cancel func aborts the upstream request and closes
// the event channel promptly (client disconnect → upstream cancellation)
// — no error event, no goroutine hang on a full channel.
func TestStream_Cancel(t *testing.T) {
	pr, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; i < 1000; i++ {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			mustWriteString(t, w, "data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"},\"finish_reason\":null}]}\n\n")
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	})

	req := chatRequest()
	req.Stream = true
	events, cancel, err := pr.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	select {
	case _, ok := <-events:
		if !ok {
			t.Fatal("channel closed before the first event")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no first event within 5s")
	}

	cancel()

	// The channel must close (not hang) — drain with a short deadline.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return // closed — contract satisfied
			}
		case <-deadline:
			t.Fatal("cancel must close the event channel promptly")
		}
	}
}

// TestStream_DispatchFailure: a pre-stream HTTP error is returned as the
// Stream dispatch error (an UpstreamError the routing layer can classify),
// not delivered on the channel.
func TestStream_DispatchFailure(t *testing.T) {
	pr, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		mustWrite(t, w, testutil.LoadGolden(t, "openai/error.json"))
	})

	req := chatRequest()
	req.Stream = true
	events, _, err := pr.Stream(context.Background(), req)
	if err == nil {
		for range events { // drain so a wrongly started pump exits
		}
		t.Fatal("Stream dispatch against a 500 must fail")
	}
	var up *provider.UpstreamError
	if !errors.As(err, &up) {
		t.Fatalf("dispatch error = %v (%T), want *provider.UpstreamError", err, err)
	}
	if up.HTTPStatus != http.StatusInternalServerError {
		t.Errorf("HTTPStatus = %d, want 500", up.HTTPStatus)
	}
}
