package routing_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/proxyllm/proxyllm/internal/provider"
	"github.com/proxyllm/proxyllm/internal/routing"
	"github.com/proxyllm/proxyllm/internal/translate/ir"
)

// Failover-executor contract tests, RED-GREEN. Derived from the phase-1
// research facts (docs/research/phase-1-best-practices.md §3, accessed
// 2026-09-10):
//
//	FO-1  Early-stream failures (before first content byte: 408/429/5xx/
//	      TLS reset) are safe to retry/fail over. Mid-stream failures are
//	      never retried or swapped — stop cleanly, surface the error.
//	FO-3  Retry budgets: max 1–2 retries per provider, max 2–3 providers
//	      total, backoff ceiling 2–4 s, total timeout including TTFT.
//	FO-5  Streaming requests are never retried once dispatched — partial
//	      data may already have been sent. (Identical to our FO-1 rule;
//	      the stream path knows only failover-before-first-event and
//	      terminal.)
//
// Executor defaults below encode FO-3: MaxRetriesPerProvider=2 (the upper
// end of 1–2), MaxProviders=3, BackoffCeiling=3s (inside 2–4 s).

// scriptedProvider serves a queue of per-attempt results for the executor
// tests (Analysis F mock discipline: behavior scripted up front, never
// derived from the code under test).
type scriptedProvider struct {
	id string
	// caps: reported Capabilities (zero value = none; needed by rows that
	// combine scripted success behavior with a capability profile).
	caps provider.Capabilities
	// completeErrs: per-attempt Complete results (nil = success; entries
	// exhausted → success).
	completeErrs []error
	// streamErr: per-attempt Stream dispatch errors.
	streamErr []error
	// streamScript: per-attempt event sequences (default: start + done).
	streamScript [][]ir.StreamEvent

	calls       int
	cancelCalls int // invocations of the cancel func handed out by Stream
}

// Calls exposes the attempt counter to the pipeline determinism suite
// (which asserts attempted providers via the interface, not the type).
func (p *scriptedProvider) Calls() int { return p.calls }

func (p *scriptedProvider) ID() string                                 { return p.id }
func (p *scriptedProvider) Capabilities() provider.Capabilities        { return p.caps }
func (p *scriptedProvider) RefreshCredentials(_ context.Context) error { return nil }

func (p *scriptedProvider) Complete(_ context.Context, _ ir.Request) (ir.Response, error) {
	i := p.calls
	p.calls++
	if i < len(p.completeErrs) && p.completeErrs[i] != nil {
		return ir.Response{}, p.completeErrs[i]
	}
	return ir.Response{ID: "ok"}, nil
}

// countingCancel returns a cancel func that records its invocations on p
// (4c leak probe: the executor must release a failed attempt's upstream
// resources when failing over).
func (p *scriptedProvider) countingCancel() func() {
	return func() { p.cancelCalls++ }
}

func (p *scriptedProvider) Stream(_ context.Context, _ ir.Request) (<-chan ir.StreamEvent, func(), error) {
	i := p.calls
	p.calls++
	if i < len(p.streamErr) && p.streamErr[i] != nil {
		return nil, p.countingCancel(), p.streamErr[i]
	}
	script := []ir.StreamEvent{
		{Type: ir.Start, Response: &ir.Response{ID: "ok"}},
		{Type: ir.Done},
	}
	if i < len(p.streamScript) && p.streamScript[i] != nil {
		script = p.streamScript[i]
	}
	ch := make(chan ir.StreamEvent, len(script))
	for _, ev := range script {
		ch <- ev
	}
	close(ch)
	return ch, p.countingCancel(), nil
}

// upErr builds a typed upstream error.
func upErr(status int, code string) error {
	return &provider.UpstreamError{HTTPStatus: status, Code: code, Message: "test"}
}

// newExecutor wires a fresh executor around the given providers.
func newExecutor(t *testing.T, provs ...provider.Provider) *routing.Executor {
	t.Helper()
	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry: regOf(provs...),
		Health:   routing.NewHealthTracker(),
		// Backoff is behavior-relevant, not timing-relevant here: no real
		// waiting in tests (Analysis F determinism).
		RetrySleeper: func(time.Duration) {},
	})
	return e
}

// regOf builds a registry holding provs.
func regOf(provs ...provider.Provider) provider.Registry {
	reg := provider.NewRegistry()
	for _, p := range provs {
		reg.Register(p)
	}
	return reg
}

// hasEvent reports whether trace contains an event of the given type.
func hasEvent(trace *routing.Trace, typ routing.EventType) bool {
	for _, ev := range trace.Events() {
		if ev.Type == typ {
			return true
		}
	}
	return false
}

// chain builds a chain of provider IDs on one model.
func chain(ids ...string) []routing.ChainEntry {
	out := make([]routing.ChainEntry, len(ids))
	for i, id := range ids {
		out[i] = routing.ChainEntry{ProviderID: id, Model: "m"}
	}
	return out
}

// TestExecutor_NonStreaming_FailoverOnServerError: 5xx fails over to the
// next chain entry (FO-2 server_error → failover fast).
func TestExecutor_NonStreaming_FailoverOnServerError(t *testing.T) {
	p1 := &scriptedProvider{id: "p1", completeErrs: []error{upErr(500, "server_error")}}
	p2 := &scriptedProvider{id: "p2"}
	e := newExecutor(t, p1, p2)

	resp, trace, err := e.Execute(context.Background(), chain("p1", "p2"), ir.Request{Model: "m"})
	if err != nil || resp.ID != "ok" {
		t.Fatalf("Execute = %+v, %v; want p2 success", resp, err)
	}
	// FO-4: attempt p1 → failed → failover → attempt p2.
	events := trace.Events()
	want := []routing.EventType{routing.EventAttempt, routing.EventFailed, routing.EventFailover, routing.EventAttempt}
	if len(events) < len(want) {
		t.Fatalf("trace events = %+v, want at least %v", events, want)
	}
	for i, wt := range want {
		if events[i].Type != wt {
			t.Errorf("event[%d] = %s, want %s (all: %+v)", i, events[i].Type, wt, events)
		}
	}
}

// TestExecutor_NonStreaming_ContentFilterNeverFailsOver: content_filter
// is terminal across the whole chain (FO-2 compliance risk — the same
// prompt triggers the same filter elsewhere).
func TestExecutor_NonStreaming_ContentFilterNeverFailsOver(t *testing.T) {
	p1 := &scriptedProvider{id: "p1", completeErrs: []error{upErr(400, "content_filter")}}
	p2 := &scriptedProvider{id: "p2"}
	e := newExecutor(t, p1, p2)

	_, _, err := e.Execute(context.Background(), chain("p1", "p2"), ir.Request{Model: "m"})
	if err == nil {
		t.Fatal("content_filter must terminate the request, not fail over")
	}
	if p2.calls != 0 {
		t.Errorf("p2 was called %d times; content_filter must never reach the next provider", p2.calls)
	}
}

// TestExecutor_NonStreaming_RateLimitRetriesThenFailsOver: 429 retries
// the same provider within the budget, then fails over once the budget
// is spent (FO-2 rate_limit → retry same, FO-3 budget).
func TestExecutor_NonStreaming_RateLimitRetriesThenFailsOver(t *testing.T) {
	p1 := &scriptedProvider{id: "p1", completeErrs: []error{
		upErr(429, ""), upErr(429, ""), upErr(429, ""),
	}}
	p2 := &scriptedProvider{id: "p2"}
	e := newExecutor(t, p1, p2)

	_, trace, err := e.Execute(context.Background(), chain("p1", "p2"), ir.Request{Model: "m"})
	if err != nil || p2.calls == 0 {
		t.Fatalf("Execute err = %v, p2.calls = %d; want success via p2 after budget exhaustion", err, p2.calls)
	}
	// FO-3: default budget is 2 retries on p1 before failover.
	if p1.calls != 3 {
		t.Errorf("p1.calls = %d, want 3 (initial + 2 retries)", p1.calls)
	}
	if !hasEvent(trace, routing.EventRetry) {
		t.Error("trace must record same-provider retries (never silent)")
	}
}

// TestExecutor_NonStreaming_RateLimitRetrySucceeds: when the same
// provider recovers within the budget, no failover happens at all.
func TestExecutor_NonStreaming_RateLimitRetrySucceeds(t *testing.T) {
	p1 := &scriptedProvider{id: "p1", completeErrs: []error{upErr(429, ""), nil}}
	p2 := &scriptedProvider{id: "p2"}
	e := newExecutor(t, p1, p2)

	resp, trace, err := e.Execute(context.Background(), chain("p1", "p2"), ir.Request{Model: "m"})
	if err != nil || resp.ID != "ok" {
		t.Fatalf("Execute = %+v, %v; want p1 success on retry", resp, err)
	}
	if p2.calls != 0 {
		t.Errorf("p2.calls = %d; recovery within budget must not fail over", p2.calls)
	}
	if !hasEvent(trace, routing.EventRetry) {
		t.Error("trace must record the retry")
	}
}

// TestExecutor_NonStreaming_BudgetExhaustionTerminal: when the whole
// chain exhausts its budgets, the last attempt's error is the terminal
// error (no silent default, no endless loop).
func TestExecutor_NonStreaming_BudgetExhaustionTerminal(t *testing.T) {
	p1 := &scriptedProvider{id: "p1", completeErrs: []error{upErr(500, "")}}
	e := newExecutor(t, p1)

	_, _, err := e.Execute(context.Background(), chain("p1"), ir.Request{Model: "m"})
	if err == nil {
		t.Fatal("budget exhaustion must surface the terminal error")
	}
}

// TestExecutor_NonStreaming_ClientErrorTerminal: 401 never retried,
// never failed over (FO-2 client_error).
func TestExecutor_NonStreaming_ClientErrorTerminal(t *testing.T) {
	p1 := &scriptedProvider{id: "p1", completeErrs: []error{upErr(401, "")}}
	p2 := &scriptedProvider{id: "p2"}
	e := newExecutor(t, p1, p2)

	_, _, err := e.Execute(context.Background(), chain("p1", "p2"), ir.Request{Model: "m"})
	if err == nil {
		t.Fatal("client error must be terminal")
	}
	if p2.calls != 0 {
		t.Errorf("p2.calls = %d; client error must never fail over", p2.calls)
	}
}

// TestExecutor_NonStreaming_HealthReportedPerAttempt: failures count
// toward the breaker; a success closes it again.
func TestExecutor_NonStreaming_HealthReportedPerAttempt(t *testing.T) {
	h := routing.NewHealthTracker()
	p1 := &scriptedProvider{id: "p1", completeErrs: []error{upErr(500, "")}}
	p2 := &scriptedProvider{id: "p2"}
	e := routing.NewExecutor(routing.ExecutorConfig{Registry: regOf(p1, p2), Health: h})

	if _, _, err := e.Execute(context.Background(), chain("p1", "p2"), ir.Request{Model: "m"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if h.Cooldown("p1") {
		t.Error("one failure must not open the breaker (threshold 3)")
	}
	h.ReportFailure("p1")
	h.ReportFailure("p1")
	if !h.Cooldown("p1") {
		t.Error("three consecutive failures must open the breaker")
	}
	if _, _, err := e.Execute(context.Background(), chain("p1", "p2"), ir.Request{Model: "m"}); err != nil {
		t.Fatalf("Execute with open breaker: %v", err)
	}
	// p1 succeeded in the second Execute → breaker closed.
	if h.Cooldown("p1") {
		t.Error("success must reset the breaker")
	}
}

// TestExecutor_NonStreaming_RetryAfterHonoredAndCapped: Retry-After from
// the upstream error drives the retry delay, capped by the FO-3 ceiling
// (O10).
func TestExecutor_NonStreaming_RetryAfterHonoredAndCapped(t *testing.T) {
	for _, tc := range []struct {
		name        string
		retryAfter  time.Duration
		wantSleep   time.Duration
		ceilingSeen bool
	}{
		{name: "advertised delay within ceiling", retryAfter: 1200 * time.Millisecond, wantSleep: 1200 * time.Millisecond},
		{name: "advertised delay capped at ceiling", retryAfter: 90 * time.Second, wantSleep: 3 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p1 := &scriptedProvider{id: "p1", completeErrs: []error{
				&provider.UpstreamError{HTTPStatus: 429, RetryAfter: tc.retryAfter, Message: "slow down"},
				nil,
			}}
			var slept []time.Duration
			e := routing.NewExecutor(routing.ExecutorConfig{
				Registry:     regOf(p1),
				Health:       routing.NewHealthTracker(),
				RetrySleeper: func(d time.Duration) { slept = append(slept, d) },
			})
			if _, _, err := e.Execute(context.Background(), chain("p1"), ir.Request{Model: "m"}); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if len(slept) != 1 || slept[0] != tc.wantSleep {
				t.Errorf("slept = %v, want exactly [%v]", slept, tc.wantSleep)
			}
		})
	}
}

// TestExecutor_NonStreaming_ExponentialBackoffWithoutRetryAfter: with no
// Retry-After, retries back off exponentially from the base, capped by
// the ceiling (FO-3).
func TestExecutor_NonStreaming_ExponentialBackoffWithoutRetryAfter(t *testing.T) {
	p1 := &scriptedProvider{id: "p1", completeErrs: []error{
		upErr(429, ""), upErr(429, ""), upErr(429, ""),
	}}
	p2 := &scriptedProvider{id: "p2"}
	var slept []time.Duration
	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(p1, p2),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(d time.Duration) { slept = append(slept, d) },
		RetryBackoff: 200 * time.Millisecond,
	})
	if _, _, err := e.Execute(context.Background(), chain("p1", "p2"), ir.Request{Model: "m"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// attempt 1 backoff = base; attempt 2 backoff = base*2.
	want := []time.Duration{200 * time.Millisecond, 400 * time.Millisecond}
	if len(slept) != len(want) {
		t.Fatalf("slept = %v, want %v", slept, want)
	}
	for i, w := range want {
		if slept[i] != w {
			t.Errorf("slept[%d] = %v, want %v", i, slept[i], w)
		}
	}
}

// TestExecutor_Stream_DispatchFailover: a dispatch error (before any
// event) fails over to the next chain entry (FO-1 early-stream retryable).
func TestExecutor_Stream_DispatchFailover(t *testing.T) {
	p1 := &scriptedProvider{id: "p1", streamErr: []error{upErr(500, "")}}
	p2 := &scriptedProvider{id: "p2"}
	e := newExecutor(t, p1, p2)

	ch, cancel, trace, err := e.ExecuteStream(context.Background(), chain("p1", "p2"), ir.Request{Model: "m"})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	defer cancel()
	for range ch {
	}
	if p2.calls == 0 {
		t.Error("dispatch failure must fail over to p2")
	}
	if !hasEvent(trace, routing.EventFailover) {
		t.Error("trace must record the failover")
	}
}

// TestExecutor_Stream_MidStreamNoSwap: once an event has been delivered,
// an error is terminal — the stream stops, no swap (FO-1/FO-5).
func TestExecutor_Stream_MidStreamNoSwap(t *testing.T) {
	p1 := &scriptedProvider{id: "p1", streamScript: [][]ir.StreamEvent{{
		{Type: ir.Start, Response: &ir.Response{ID: "s"}},
		{Type: ir.Error, Error: &ir.UpstreamError{Message: "mid-stream"}},
	}}}
	p2 := &scriptedProvider{id: "p2"}
	e := newExecutor(t, p1, p2)

	ch, cancel, trace, err := e.ExecuteStream(context.Background(), chain("p1", "p2"), ir.Request{Model: "m"})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	defer cancel()
	sawError := false
	for ev := range ch {
		if ev.Type == ir.Error {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("mid-stream error must surface as a terminal Error event")
	}
	if p2.calls != 0 {
		t.Errorf("p2.calls = %d; mid-stream failure must never swap providers", p2.calls)
	}
	if hasEvent(trace, routing.EventFailover) {
		t.Error("trace must not show a failover after streaming started")
	}
}

// TestExecutor_Stream_NeverRetriedOnceDispatched: FO-5 — even a
// rate-limit-class dispatch failure on a stream never retries the same
// provider; the request fails over (or terminates) instead.
func TestExecutor_Stream_NeverRetriedOnceDispatched(t *testing.T) {
	p1 := &scriptedProvider{id: "p1", streamErr: []error{upErr(429, ""), upErr(429, "")}}
	p2 := &scriptedProvider{id: "p2"}
	e := newExecutor(t, p1, p2)

	_, _, trace, err := e.ExecuteStream(context.Background(), chain("p1", "p2"), ir.Request{Model: "m"})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	if p1.calls != 1 {
		t.Errorf("p1.calls = %d, want exactly 1 — streams are never retried once dispatched (FO-5)", p1.calls)
	}
	if hasEvent(trace, routing.EventRetry) {
		t.Error("trace must not record a stream retry (FO-5)")
	}
}

// TestExecutor_Stream_SingleProviderDispatchExhaustion: with a
// single-entry chain, a stream dispatch error terminates the request.
func TestExecutor_Stream_SingleProviderDispatchExhaustion(t *testing.T) {
	p1 := &scriptedProvider{id: "p1", streamErr: []error{errors.New("connection refused")}}
	e := newExecutor(t, p1)

	_, _, _, err := e.ExecuteStream(context.Background(), chain("p1"), ir.Request{Model: "m"})
	if err == nil {
		t.Fatal("dispatch failure with an exhausted chain must be terminal")
	}
}

// TestExecutor_Stream_CancelReleasedOnFailover: a dispatch-failure
// failover must release the failed attempt's upstream resources — the
// cancel func returned alongside the dispatch error must be invoked (4c;
// FO-1 failover must not leak the aborted attempt's connection).
func TestExecutor_Stream_CancelReleasedOnFailover(t *testing.T) {
	p1 := &scriptedProvider{id: "p1", streamErr: []error{upErr(500, "")}}
	p2 := &scriptedProvider{id: "p2"}
	e := newExecutor(t, p1, p2)

	ch, cancel, _, err := e.ExecuteStream(context.Background(), chain("p1", "p2"), ir.Request{Model: "m"})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	defer cancel()
	for range ch {
	}
	if p1.cancelCalls != 1 {
		t.Errorf("p1 cancel calls = %d, want 1 — the failed attempt's cancel must run on failover", p1.cancelCalls)
	}
	if p2.cancelCalls != 0 {
		t.Errorf("p2 cancel calls = %d, want 0 — the serving attempt's cancel belongs to the caller", p2.cancelCalls)
	}
}

// TestExecutor_ReportStreamOutcome_MidStreamFailureCounts: 4a — a
// dispatch that succeeds but fails mid-stream must still count as a
// breaker failure. ExecuteStream reports success at dispatch (FO-5: the
// stream is never retried); once httpapi has relayed the stream it calls
// ReportStreamOutcome to correct the record. The caller of this method
// is the HTTP layer, so the test drives it directly, exactly as httpapi
// will (dispatch success → drain → outcome callback).
func TestExecutor_ReportStreamOutcome_MidStreamFailureCounts(t *testing.T) {
	h := routing.NewHealthTracker()
	p1 := &scriptedProvider{id: "p1", streamScript: [][]ir.StreamEvent{{
		{Type: ir.Start, Response: &ir.Response{ID: "s"}},
		{Type: ir.Error, Error: &ir.UpstreamError{Message: "mid-stream"}},
	}}}
	e := routing.NewExecutor(routing.ExecutorConfig{Registry: regOf(p1), Health: h})

	ch, cancel, _, err := e.ExecuteStream(context.Background(), chain("p1"), ir.Request{Model: "m"})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	for range ch {
	}
	cancel()

	// Dispatch success was reported; the breaker must not be open yet.
	if h.Cooldown("p1") {
		t.Fatal("breaker open before the mid-stream outcome was reported")
	}

	// httpapi relays the stream, sees the Error event, and reports the
	// outcome with the upstream error (same threshold-3 pattern as the
	// non-streaming health test).
	e.ReportStreamOutcome("p1", &ir.UpstreamError{Message: "mid-stream"})
	if h.Cooldown("p1") {
		t.Fatal("one failure must not open the breaker (threshold 3)")
	}
	h.ReportFailure("p1")
	h.ReportFailure("p1")
	if !h.Cooldown("p1") {
		t.Error("mid-stream failure must count toward the breaker like any other failure")
	}
}

// TestExecutor_ReportStreamOutcome_CleanStreamKeepsSuccess: a clean
// stream outcome reports success — and stays idempotent with the
// dispatch-time success report (FO-5: no double-counting, no flap).
func TestExecutor_ReportStreamOutcome_CleanStreamKeepsSuccess(t *testing.T) {
	h := routing.NewHealthTracker()
	p1 := &scriptedProvider{id: "p1"}
	e := routing.NewExecutor(routing.ExecutorConfig{Registry: regOf(p1), Health: h})

	ch, cancel, _, err := e.ExecuteStream(context.Background(), chain("p1"), ir.Request{Model: "m"})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	for range ch {
	}
	cancel()

	// Two failures so far; a clean outcome must NOT open the breaker —
	// the dispatch success and the outcome success agree.
	h.ReportFailure("p1")
	h.ReportFailure("p1")
	e.ReportStreamOutcome("p1", nil)
	if h.Cooldown("p1") {
		t.Error("clean stream outcome must leave the breaker closed (2 failures, threshold 3)")
	}
}

// TestExecutor_ReportStreamOutcome_EmptyProviderNoop: httpapi resolves
// the provider from the trace; an empty ID (no attempt recorded) must be
// a no-op, not a panic or a report against "".
func TestExecutor_ReportStreamOutcome_EmptyProviderNoop(t *testing.T) {
	h := routing.NewHealthTracker()
	p1 := &scriptedProvider{id: "p1"}
	e := routing.NewExecutor(routing.ExecutorConfig{Registry: regOf(p1), Health: h})

	e.ReportStreamOutcome("", &ir.UpstreamError{Message: "no provider"})
	e.ReportStreamOutcome("", nil)
	if h.Cooldown("") {
		t.Error("empty provider ID must not register any health entry")
	}
}

// TestExecutor_Stream_CancelReleasedOnRetrySameClassFailover: FO-5 turns
// a 429-class dispatch failure into a failover too — that branch must
// equally release the failed attempt's cancel (4c).
func TestExecutor_Stream_CancelReleasedOnRetrySameClassFailover(t *testing.T) {
	p1 := &scriptedProvider{id: "p1", streamErr: []error{upErr(429, "")}}
	p2 := &scriptedProvider{id: "p2"}
	e := newExecutor(t, p1, p2)

	ch, cancel, _, err := e.ExecuteStream(context.Background(), chain("p1", "p2"), ir.Request{Model: "m"})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	defer cancel()
	for range ch {
	}
	if p1.cancelCalls != 1 {
		t.Errorf("p1 cancel calls = %d, want 1 — rate_limit failover must release the failed attempt's cancel", p1.cancelCalls)
	}
}
