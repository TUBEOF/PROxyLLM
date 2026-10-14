package ollamacloud_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/provider/ollamacloud"
	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/openai"
	"github.com/tubeof/proxyllm/internal/translate/testutil"
)

// Ollama Cloud provider contract tests, RED-GREEN.
// The cloud provider rides the OpenAI-compatible surface
// (base https://ollama.com + /v1 paths, docs.ollama.com/api/
// openai-compatibility, accessed 2026-09-22) — the native /api/chat wire
// fragmented interleaved thinking into separate content blocks (a Claude
// Code rendering bug). Expectations derive from the OpenAI-compat wire
// facts and the openai golden fixtures — never from provider code.
//
// Wire facts pinned here:
//   - chat is POST {base}/v1/chat/completions with {model, messages,
//     stream}.
//   - auth is `Authorization: Bearer $OLLAMA_API_KEY`.
//   - streaming is data-only SSE with the [DONE] sentinel.
//   - non-2xx bodies decode in the OpenAI error shape.
//   - same error mapping and cancellation contracts as every
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

// mustWriteString is mustWrite for string payloads (SSE lines).
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
		Model: "gemma4:31b",
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

// TestComplete_RoundTrip: a 2xx /v1/chat/completions body decodes to the
// IR via the OpenAI codec; auth is Bearer; the body carries
// "stream":false (the OpenAI-compat endpoint defaults to false, but the
// explicit field keeps the contract visible).
func TestComplete_RoundTrip(t *testing.T) {
	pr := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testAPIKey {
			t.Errorf("Authorization = %q, want Bearer %q", got, testAPIKey)
		}
		body := decodeSentBody(t, r)
		// The OpenAI-compat endpoint defaults stream to false; the codec
		// omits the field for non-streaming requests (custom-openai
		// convention) — absence IS false here, unlike the native wire's
		// always-true default.
		if v, ok := body["stream"]; ok && v != false {
			t.Errorf("request stream = %v, want false (or omitted)", v)
		}
		if body["model"] != "gemma4:31b" {
			t.Errorf("request model = %v, want gemma4:31b", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		mustWrite(t, w, testutil.LoadGolden(t, "openai/chat-response.json"))
	})

	resp, err := pr.Complete(context.Background(), chatRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	want, err := openai.Codec{}.DecodeResponse(testutil.LoadGolden(t, "openai/chat-response.json"))
	if err != nil {
		t.Fatalf("decode golden response: %v", err)
	}
	// Compare content-by-content: the provider's Complete path is codec
	// identity, so a DeepEqual against the codec's own decode is the pin.
	if resp.ID != want.ID || resp.Model != want.Model || resp.StopReason != want.StopReason {
		t.Errorf("Complete response = %+v, want golden decode %+v", resp, want)
	}
	if len(resp.Content) != len(want.Content) {
		t.Fatalf("content parts = %d, want %d", len(resp.Content), len(want.Content))
	}
	for i := range resp.Content {
		if !partEqual(resp.Content[i], want.Content[i]) {
			t.Errorf("content[%d] = %+v, want %+v", i, resp.Content[i], want.Content[i])
		}
	}
	if resp.Usage != want.Usage {
		t.Errorf("usage = %+v, want %+v", resp.Usage, want.Usage)
	}
}

// partEqual compares two IR parts (a switch instead of ==: ToolCallPart
// carries json.RawMessage and would panic on a == compare).
func partEqual(a, b ir.Part) bool {
	switch pa := a.(type) {
	case ir.TextPart:
		pb, ok := b.(ir.TextPart)
		return ok && pa == pb
	case ir.ThinkingPart:
		pb, ok := b.(ir.ThinkingPart)
		return ok && pa == pb
	case ir.ToolCallPart:
		pb, ok := b.(ir.ToolCallPart)
		return ok && pa.ID == pb.ID && pa.Name == pb.Name && string(pa.Input) == string(pb.Input)
	default:
		return false
	}
}

// TestComplete_ReasoningResponse: a thinking-model body decodes with the
// trace as a ThinkingPart before the text (the reasoning_content fixture;
// the OpenAI-codec contract is exercised in detail there — this pins the
// provider plumbing).
func TestComplete_ReasoningResponse(t *testing.T) {
	pr := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mustWrite(t, w, testutil.LoadGolden(t, "openai/chat-response-reasoning.json"))
	})

	resp, err := pr.Complete(context.Background(), chatRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("content = %d parts, want 2 (thinking, text)", len(resp.Content))
	}
	if _, ok := resp.Content[0].(ir.ThinkingPart); !ok {
		t.Errorf("content[0] = %T, want ir.ThinkingPart", resp.Content[0])
	}
	if _, ok := resp.Content[1].(ir.TextPart); !ok {
		t.Errorf("content[1] = %T, want ir.TextPart", resp.Content[1])
	}
}

// TestComplete_UpstreamError: a non-2xx maps to *provider.UpstreamError
// with HTTP status + body error fields + Retry-After. The
// OpenAI-compat surface answers `{"error":{"message":...}}` — Code/Type
// stay empty, Message carries the text.
func TestComplete_UpstreamError(t *testing.T) {
	pr := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		mustWriteString(t, w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
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
		t.Errorf("Message = %q, want the OpenAI-compat error text", up.Message)
	}
	if up.RetryAfter != 3*time.Second {
		t.Errorf("RetryAfter = %v, want 3s", up.RetryAfter)
	}
}

// TestStream_Events: the SSE fixture replays to the canonical event
// sequence from the .expected.json fixture; the body carries stream:true.
func TestStream_Events(t *testing.T) {
	pr := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeSentBody(t, r)
		if body["stream"] != true {
			t.Errorf("request stream = %v, want true", body["stream"])
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

// TestStream_ReasoningInterleaved: the provider-level pin of the
// 2026-09-22 rendering bug — an interleaved reasoning stream must decode
// to one text block + one thinking block (the OpenAI-codec fixture pins
// the full event sequence; here the reasoning field must not be dropped
// on the provider path).
func TestStream_ReasoningInterleaved(t *testing.T) {
	pr := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		mustWrite(t, w, testutil.LoadGolden(t, "openai/chat-stream-reasoning.sse"))
	})

	req := chatRequest()
	req.Stream = true
	events, cancel, err := pr.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer cancel()

	got := drainEvents(t, events)
	sawThinking := false
	textBlocks, thinkBlocks := 0, 0
	for _, ev := range got {
		if ev.Type != ir.BlockStart {
			continue
		}
		switch ev.Part.(type) {
		case ir.TextPart:
			textBlocks++
		case ir.ThinkingPart:
			thinkBlocks++
			sawThinking = true
		}
	}
	if !sawThinking {
		t.Fatal("reasoning trace lost on the provider path")
	}
	if textBlocks != 1 || thinkBlocks != 1 {
		t.Errorf("blocks: %d text / %d thinking, want 1/1 (interleaved reasoning must not fragment the text)", textBlocks, thinkBlocks)
	}
}

// TestStream_Cancel: cancel aborts the upstream request and closes the
// channel.
func TestStream_Cancel(t *testing.T) {
	pr := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; i < 1000; i++ {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			mustWriteString(t, w, "data: {\"id\":\"c1\",\"model\":\"gemma4:31b\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"},\"finish_reason\":null}]}\n\n")
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
			t.Fatal("cancel must close the event channel promptly")
		}
	}
}

// TestListModels_OpenAIShape: /v1/models decodes the OpenAI list shape
// ("data":[{"id":...}]); empty and duplicate IDs are skipped.
func TestListModels_OpenAIShape(t *testing.T) {
	pr := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			if got := r.Header.Get("Authorization"); got != "Bearer "+testAPIKey {
				t.Errorf("Authorization = %q, want Bearer %q", got, testAPIKey)
			}
			w.Header().Set("Content-Type", "application/json")
			mustWriteString(t, w, `{"object":"list","data":[`+
				`{"id":"gemma4:31b","object":"model","owned_by":"library"},`+
				`{"id":"gpt-oss:20b","object":"model","owned_by":"library"},`+
				`{"id":"gemma4:31b","object":"model","owned_by":"dup"},`+
				`{"id":"","object":"model","owned_by":"empty"}]}`)
		default:
			// The enrichment probes (/api/show) are best-effort; this
			// handler fails them — models must still import with zero
			// facts (detection degrades explicitly per model).
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"model not found"}`))
		}
	})

	models, err := pr.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v, want 2 entries (deduped, empties skipped)", models)
	}
	if models[0].Name != "gemma4:31b" || models[1].Name != "gpt-oss:20b" {
		t.Errorf("model names = %q/%q, want gemma4:31b/gpt-oss:20b", models[0].Name, models[1].Name)
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
