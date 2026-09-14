package ollamacloud_test

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

	"github.com/proxyllm/proxyllm/internal/provider"
	"github.com/proxyllm/proxyllm/internal/provider/ollamacloud"
	"github.com/proxyllm/proxyllm/internal/translate/ir"
	"github.com/proxyllm/proxyllm/internal/translate/ollama"
	"github.com/proxyllm/proxyllm/internal/translate/testutil"
)

// Ollama Cloud provider contract tests, RED-GREEN (plan §4.1, Analysis F).
// Expectations derive from the researched wire facts (accessed 2026-09-10)
// and the golden fixtures pinned from the Ollama native /api/chat wire
// format — never from provider code behavior.
//
// Wire facts pinned here:
//   - L1: hosted native API is POST {base}/api/chat with {model, messages,
//     stream}.
//   - L2: auth is `Authorization: Bearer $OLLAMA_API_KEY`.
//   - L8: stream is ALWAYS explicit in the request body — `"stream":false`
//     must appear on non-streaming requests (Ollama defaults to true when
//     absent).
//   - L11: usage maps prompt_eval_count / eval_count /
//     prompt_eval_cached_count.
//   - FO-2/C2/FO-1: same error mapping and cancellation contracts as every
//     HTTP provider.

const testAPIKey = "ollama-key-456" //nolint:gosec // dummy fixture key, never a real credential

// mustWrite is the test-handler write helper: a write failure inside a
// scripted httptest handler is always a fatal test condition.
func mustWrite(t *testing.T, w io.Writer, data []byte) {
	t.Helper()
	if _, err := w.Write(data); err != nil {
		t.Fatalf("test handler write: %v", err)
	}
}

// mustWriteString is mustWrite for string payloads (NDJSON lines).
func mustWriteString(t *testing.T, w io.Writer, s string) {
	t.Helper()
	if _, err := io.WriteString(w, s); err != nil {
		t.Fatalf("test handler write: %v", err)
	}
}

func newProvider(t *testing.T, handler http.HandlerFunc) *ollamacloud.Provider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return ollamacloud.New("ollama-cloud", srv.URL, testAPIKey, provider.Capabilities{
		Streaming: true,
		Tools:     true,
	})
}

func chatRequest() ir.Request {
	return ir.Request{
		Model: "llama3.3",
		Messages: []ir.Message{{
			Role:  ir.RoleUser,
			Parts: []ir.Part{ir.TextPart{Text: "Say hello"}},
		}},
	}
}

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

// TestComplete_RoundTrip: a 2xx /api/chat body decodes to the IR via the
// Ollama codec; auth is Bearer (L2); the body carries an explicit
// `"stream":false` (L8).
func TestComplete_RoundTrip(t *testing.T) {
	pr := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/chat" {
			t.Errorf("path = %s, want /api/chat (L1)", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testAPIKey {
			t.Errorf("Authorization = %q, want Bearer %q (L2)", got, testAPIKey)
		}
		body := decodeSentBody(t, r)
		if body["stream"] != false {
			t.Errorf("request stream = %v, want explicit false (L8)", body["stream"])
		}
		if body["model"] != "llama3.3" {
			t.Errorf("request model = %v, want llama3.3", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		mustWrite(t, w, testutil.LoadGolden(t, "ollama/chat-response.json"))
	})

	resp, err := pr.Complete(context.Background(), chatRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	want, err := ollama.Codec{}.DecodeResponse(testutil.LoadGolden(t, "ollama/chat-response.json"))
	if err != nil {
		t.Fatalf("decode golden response: %v", err)
	}
	if !reflect.DeepEqual(resp, want) {
		t.Errorf("Complete response = %+v, want golden decode %+v", resp, want)
	}
}

// TestComplete_UpstreamError: a non-2xx maps to *provider.UpstreamError
// with HTTP status + body error fields + Retry-After (FO-2, O10). Ollama
// error bodies are `{"error":"..."}` — Code/Type stay empty, Message
// carries the text.
func TestComplete_UpstreamError(t *testing.T) {
	pr := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		mustWriteString(t, w, `{"error":"rate limited"}`)
	})

	_, err := pr.Complete(context.Background(), chatRequest())
	var up *provider.UpstreamError
	if !errors.As(err, &up) {
		t.Fatalf("Complete error = %v (%T), want *provider.UpstreamError", err, err)
	}
	if up.HTTPStatus != http.StatusTooManyRequests {
		t.Errorf("HTTPStatus = %d, want %d", up.HTTPStatus, http.StatusTooManyRequests)
	}
	if up.Message != "rate limited" {
		t.Errorf("Message = %q, want the Ollama error text", up.Message)
	}
	if up.RetryAfter != 3*time.Second {
		t.Errorf("RetryAfter = %v, want 3s", up.RetryAfter)
	}
}

// TestStream_Events: the NDJSON fixture replays to the canonical event
// sequence from the .expected.json fixture; the body carries stream:true.
func TestStream_Events(t *testing.T) {
	pr := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeSentBody(t, r)
		if body["stream"] != true {
			t.Errorf("request stream = %v, want true", body["stream"])
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		mustWrite(t, w, testutil.LoadGolden(t, "ollama/chat-stream.ndjson"))
	})

	req := chatRequest()
	req.Stream = true
	events, cancel, err := pr.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer cancel()

	got := drainEvents(t, events)
	want := testutil.LoadExpectedEvents(t, "ollama/chat-stream.ndjson.expected.json")
	testutil.AssertEventsEqual(t, "ollama/chat-stream.ndjson", want, got)
}

// TestStream_Cancel: cancel aborts the upstream request and closes the
// channel (C2).
func TestStream_Cancel(t *testing.T) {
	pr := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher := w.(http.Flusher)
		for i := 0; i < 1000; i++ {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			mustWriteString(t, w, `{"model":"llama3.3","message":{"role":"assistant","content":"a"},"done":false}`+"\n")
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
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("cancel must close the event channel promptly (C2)")
		}
	}
}

// TestProvider_IdentityAndCapabilities: ID and capabilities round-trip from
// construction; RefreshCredentials is a no-op for static-key providers.
func TestProvider_IdentityAndCapabilities(t *testing.T) {
	pr := newProvider(t, func(w http.ResponseWriter, r *http.Request) {})
	if pr.ID() != "ollama-cloud" {
		t.Errorf("ID() = %q, want ollama-cloud", pr.ID())
	}
	caps := pr.Capabilities()
	if !caps.Streaming || !caps.Tools {
		t.Errorf("Capabilities() = %+v, want Streaming+Tools from construction", caps)
	}
	if err := pr.RefreshCredentials(context.Background()); err != nil {
		t.Errorf("RefreshCredentials on a static-key provider must be a no-op, got %v", err)
	}
}
