package mockprovider

// — the scripted-mock test matrix: every scenario the
// httpapi, failover, and cancellation suites need, exercised here at the
// transport boundary over real HTTP. Expectations derive exclusively from
// the golden fixtures and the OpenAI wire facts — never from provider
// code behavior.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/translate/ir"
	ttestutil "github.com/tubeof/proxyllm/internal/translate/testutil"
)

// initialChunk is a spec-conform first chat.completion.chunk: role
// announcement with empty content, finish_reason null.
const initialChunk = `{"id":"chatcmpl-mock","object":"chat.completion.chunk","model":"mock-model","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`

// finishChunk carries finish_reason "stop".
const finishChunk = `{"id":"chatcmpl-mock","object":"chat.completion.chunk","model":"mock-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`

// textDelta builds one content-delta chunk carrying s.
func textDelta(s string) string {
	b, _ := json.Marshal(s)
	return `{"id":"chatcmpl-mock","object":"chat.completion.chunk","model":"mock-model","choices":[{"index":0,"delta":{"content":` + string(b) + `},"finish_reason":null}]}`
}

// drain reads the canonical event channel until close.
func drain(ch <-chan ir.StreamEvent) []ir.StreamEvent {
	var out []ir.StreamEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

func streamCaps() provider.Capabilities {
	return provider.Capabilities{Streaming: true, Tools: true}
}

// 1. Non-streaming round trip against the golden chat-response fixture.
func TestComplete_GoldenRoundTrip(t *testing.T) {
	srv := New(t)
	srv.Enqueue(NewScript().JSONResponse(http.StatusOK,
		string(ttestutil.LoadGolden(t, "openai/chat-response.json")), nil))
	p := srv.Provider("mock", streamCaps())

	resp, err := p.Complete(context.Background(), ir.Request{Model: "gpt-5"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.ID != "chatcmpl-abc123" || resp.Model != "gpt-5" {
		t.Errorf("id/model = %q/%q, want chatcmpl-abc123/gpt-5", resp.ID, resp.Model)
	}
	if resp.StopReason != ir.StopToolUse {
		t.Errorf("stop_reason = %q, want %q", resp.StopReason, ir.StopToolUse)
	}
	if resp.Usage.InputTokens != 1250 || resp.Usage.OutputTokens != 77 || resp.Usage.CacheReadInputTokens != 8192 {
		t.Errorf("usage = %+v, want input 1250 / output 77 / cache_read 8192", resp.Usage)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("content parts = %d, want 2 (text + tool_use)", len(resp.Content))
	}
	tp, ok := resp.Content[0].(ir.TextPart)
	if !ok || tp.Text != "I'll check the weather for you." {
		t.Errorf("content[0] = %#v, want text part with fixture text", resp.Content[0])
	}
	tc, ok := resp.Content[1].(ir.ToolCallPart)
	if !ok || tc.ID != "call_abc123" || tc.Name != "get_weather" {
		t.Errorf("content[1] = %#v, want tool_use call_abc123/get_weather", resp.Content[1])
	}
	if want := `{"location":"San Francisco, CA","unit":"celsius"}`; string(tc.Input) != want {
		t.Errorf("tool input = %s, want %s", tc.Input, want)
	}

	reqs := srv.Requests()
	if len(reqs) != 1 {
		t.Fatalf("recorded requests = %d, want 1", len(reqs))
	}
	r0 := reqs[0]
	if r0.Path != "/chat/completions" {
		t.Errorf("request path = %q, want /chat/completions", r0.Path)
	}
	if got := r0.Header.Get("Authorization"); got != "Bearer mock-key" {
		t.Errorf("Authorization = %q, want Bearer mock-key", got)
	}
	if r0.Stream {
		t.Errorf("recorded non-stream request as stream=true")
	}
	if !strings.Contains(r0.Body, `"model":"gpt-5"`) {
		t.Errorf("request body missing model: %s", r0.Body)
	}
}

// 2. Streaming round trip: the golden chat-stream.sse frames served verbatim
// must decode through the real provider into the golden canonical sequence.
func TestStream_GoldenRoundTrip(t *testing.T) {
	srv := New(t)
	srv.Enqueue(NewScript().SSEFrames(
		string(ttestutil.LoadGolden(t, "openai/chat-stream.sse"))))
	p := srv.Provider("mock", streamCaps())

	events, cancel, err := p.Stream(context.Background(), ir.Request{Model: "gpt-5", Stream: true})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer cancel()

	got := drain(events)
	want := ttestutil.LoadExpectedEvents(t, "openai/chat-stream.expected.json")
	ttestutil.AssertEventsEqual(t, "openai/chat-stream.expected.json", want, got)

	if reqs := srv.Requests(); len(reqs) != 1 || !reqs[0].Stream {
		t.Errorf("stream request not recorded as stream=true: %+v", reqs)
	}
}

// 3. Comments relay and unknown event tolerance (discipline): comment
// frames become ir.Comment events; an unknown named event with a decodable
// JSON payload must not break the stream.
func TestStream_CommentAndUnknownEventTolerance(t *testing.T) {
	srv := New(t)
	srv.Enqueue(NewScript().
		Chunk(initialChunk).
		Comment("keep-alive").
		NamedEvent("future_event", `{"unknown":"shape"}`).
		Chunk(textDelta("Hi")).
		Chunk("[DONE]"))
	p := srv.Provider("mock", streamCaps())

	events, cancel, err := p.Stream(context.Background(), ir.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer cancel()

	var comments, done, deltas int
	var text string
	for _, ev := range drain(events) {
		switch ev.Type {
		case ir.Comment:
			comments++
		case ir.Delta:
			deltas++
			text += ev.Delta.Text
		case ir.Done:
			done++
		case ir.Error:
			t.Errorf("unexpected error event: %+v", ev.Error)
		}
	}
	if comments != 1 {
		t.Errorf("comment events = %d, want 1", comments)
	}
	if deltas != 1 || text != "Hi" {
		t.Errorf("text deltas = %d (%q), want 1 (\"Hi\")", deltas, text)
	}
	if done != 1 {
		t.Errorf("done events = %d, want 1", done)
	}
}

// 4. Mid-stream upstream error: an error chunk is terminal — an
// ir.Error event, channel close, and never a Done after the error.
func TestStream_MidStreamErrorIsTerminal(t *testing.T) {
	srv := New(t)
	srv.Enqueue(NewScript().
		Chunk(initialChunk).
		Chunk(textDelta("partial")).
		Chunk(`{"error":{"message":"Overloaded","type":"overloaded_error"}}`))
	p := srv.Provider("mock", streamCaps())

	events, cancel, err := p.Stream(context.Background(), ir.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer cancel()

	got := drain(events)
	var errSeen bool
	for _, ev := range got {
		if ev.Type == ir.Done {
			t.Error("Done event after mid-stream error")
		}
		if ev.Type == ir.Error {
			errSeen = true
			if ev.Error == nil || ev.Error.Type != "overloaded_error" {
				t.Errorf("error event = %+v, want type overloaded_error", ev.Error)
			}
		}
	}
	if !errSeen {
		t.Error("no ir.Error event in stream")
	}
}

// 5. Mid-stream connection abort: the transport failure must surface
// as a terminal ir.Error event, never a silent close or a Done.
func TestStream_MidStreamAbortSurfacesErrorEvent(t *testing.T) {
	srv := New(t)
	srv.Enqueue(NewScript().
		Chunk(initialChunk).
		Chunk(textDelta("x")).
		Abort())
	p := srv.Provider("mock", streamCaps())

	events, cancel, err := p.Stream(context.Background(), ir.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer cancel()

	got := drain(events)
	if len(got) == 0 {
		t.Fatal("no events before channel close")
	}
	for _, ev := range got {
		if ev.Type == ir.Done {
			t.Error("Done event after mid-stream abort")
		}
	}
	last := got[len(got)-1]
	if last.Type != ir.Error || last.Error == nil {
		t.Errorf("last event = %+v, want terminal ir.Error", last)
	}
}

// 6. Dispatch failure: a non-2xx before any streamed byte maps to the
// typed UpstreamError with both classification tiers + Retry-After.
func TestStream_DispatchErrorMapsToUpstreamError(t *testing.T) {
	srv := New(t)
	srv.Enqueue(NewScript().Fail(http.StatusTooManyRequests,
		string(ttestutil.LoadGolden(t, "openai/error.json")), "2"))
	p := srv.Provider("mock", streamCaps())

	_, _, err := p.Stream(context.Background(), ir.Request{Model: "m", Stream: true})
	var up *provider.UpstreamError
	if !errors.As(err, &up) {
		t.Fatalf("Stream error = %v, want *provider.UpstreamError", err)
	}
	if up.HTTPStatus != http.StatusTooManyRequests {
		t.Errorf("HTTPStatus = %d, want 429", up.HTTPStatus)
	}
	if up.Type != "rate_limit_exceeded" || up.Code != "rate_limit_exceeded" {
		t.Errorf("type/code = %q/%q, want rate_limit_exceeded", up.Type, up.Code)
	}
	if up.RetryAfter != 2*time.Second {
		t.Errorf("RetryAfter = %v, want 2s", up.RetryAfter)
	}
	if !strings.Contains(up.Message, "Rate limit reached") {
		t.Errorf("Message = %q, want fixture message", up.Message)
	}
}

// 7. Transport failure (tier 1): no response at all must not surface
// as an UpstreamError — routing classifies zero-status separately.
func TestComplete_TransportErrorIsNotUpstreamError(t *testing.T) {
	srv := New(t)
	p := srv.Provider("mock", streamCaps())
	srv.Close()

	_, err := p.Complete(context.Background(), ir.Request{Model: "m"})
	if err == nil {
		t.Fatal("Complete on closed upstream: no error")
	}
	var up *provider.UpstreamError
	if errors.As(err, &up) {
		t.Errorf("transport failure surfaced as UpstreamError (status %d)", up.HTTPStatus)
	}
}

// 8. Cancellation: cancel must close the event channel promptly; the
// server-side delay is interrupted by the request context, not awaited.
func TestStream_CancelClosesChannel(t *testing.T) {
	srv := New(t)
	srv.Enqueue(NewScript().
		Chunk(initialChunk).
		Delay(10 * time.Second). // interrupted by request-context cancellation
		Chunk("[DONE]"))
	p := srv.Provider("mock", streamCaps())

	events, cancel, err := p.Stream(context.Background(), ir.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer cancel()

	select {
	case _, ok := <-events:
		if !ok {
			t.Fatal("channel closed before any event")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no first event within 2s")
	}
	cancel()

	closed := make(chan struct{})
	go func() {
		for range events {
		}
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("stream channel did not close within 2s of cancel")
	}
}

// 9. Slow-consumer backpressure: a consumer slower than the producer still
// receives every event, in order, through the bounded channel.
func TestStream_SlowConsumerReceivesAllFrames(t *testing.T) {
	srv := New(t)
	sc := NewScript().Chunk(initialChunk)
	for _, s := range []string{"a", "b", "c", "d", "e"} {
		sc = sc.Chunk(textDelta(s))
	}
	sc = sc.Chunk(finishChunk).Chunk("[DONE]")
	srv.Enqueue(sc)
	p := srv.Provider("mock", streamCaps())

	events, cancel, err := p.Stream(context.Background(), ir.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer cancel()

	var text string
	var done bool
	for ev := range events {
		if ev.Type == ir.Delta && ev.Delta.Kind == ir.DeltaText {
			text += ev.Delta.Text
			time.Sleep(10 * time.Millisecond) // slow consumer
		}
		if ev.Type == ir.Done {
			done = true
		}
	}
	if text != "abcde" {
		t.Errorf("assembled text = %q, want \"abcde\"", text)
	}
	if !done {
		t.Error("stream did not reach Done")
	}
}

// 10. Scripted queue semantics: scripts are consumed FIFO, one per request —
// the material for same-provider retry and failover scenarios.
func TestScriptQueue_FIFO(t *testing.T) {
	srv := New(t)
	srv.Enqueue(NewScript().Fail(http.StatusInternalServerError,
		`{"error":{"message":"boom","type":"server_error"}}`, ""))
	srv.Enqueue(NewScript().JSONResponse(http.StatusOK,
		string(ttestutil.LoadGolden(t, "openai/chat-response.json")), nil))
	p := srv.Provider("mock", streamCaps())

	_, err := p.Complete(context.Background(), ir.Request{Model: "m"})
	var up *provider.UpstreamError
	if !errors.As(err, &up) {
		t.Fatalf("first request error = %v, want *provider.UpstreamError", err)
	}
	if up.HTTPStatus != http.StatusInternalServerError {
		t.Errorf("first request status = %d, want 500", up.HTTPStatus)
	}

	resp, err := p.Complete(context.Background(), ir.Request{Model: "m"})
	if err != nil {
		t.Fatalf("second request: %v", err)
	}
	if resp.ID != "chatcmpl-abc123" {
		t.Errorf("second request id = %q, want golden fixture response", resp.ID)
	}
}
