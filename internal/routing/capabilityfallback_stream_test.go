package routing_test

// Streaming capability-fallback contract tests (2026-09-21 automatic
// capabilities): the executor-side model fallback applies to STREAMS the
// same way as completes — a chain whose every DISPATCH failed with a
// capability_gap (a 400 image rejection happens before the first byte)
// gets one walk over the configured fallback models via p.Stream. FO-1 is
// untouched: a dispatch success is never re-routed, and a mid-stream
// failure rides the channel terminally.

import (
	"context"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// streamGapErr is one stream-dispatch capability-gap rejection.
func streamGapErr() error {
	return &provider.UpstreamError{HTTPStatus: 400, Message: "image input is not supported by this model"}
}

func TestExecutor_CapabilityFallback_StreamRescuesGapExhaustedChain(t *testing.T) {
	main := &scriptedProvider{id: "main", streamErr: []error{streamGapErr()}}
	fallback := &scriptedProvider{id: "fbm"}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, fallback),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "fbm", Model: "fbm"},
		},
	})

	ch, cancel, trace, err := e.ExecuteStream(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, ir.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream: %v (the fallback model must rescue the gap)", err)
	}
	defer cancel()
	var events []ir.StreamEvent
	for ev := range ch {
		events = append(events, ev)
	}
	if len(events) == 0 || events[0].Type != ir.Start {
		t.Fatalf("stream events = %+v, want the fallback model's start event", events)
	}
	if fallback.calls == 0 {
		t.Error("fallback model was never attempted")
	}
	// Never silent: the consultation and the failover are traced.
	sawFallback, sawFailover := false, false
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventCapabilityFallback && ev.Capability == "vision" {
			sawFallback = true
		}
		if ev.Type == routing.EventFailover && ev.To == "fbm" {
			sawFailover = true
		}
	}
	if !sawFallback || !sawFailover {
		t.Errorf("trace missing fallback consultation/failover: fallback=%v failover=%v", sawFallback, sawFailover)
	}
}

func TestExecutor_CapabilityFallback_StreamOtherFailuresDoNotTrigger(t *testing.T) {
	// A 401 dispatch failure is the credentials' problem — no model
	// fallback (identical contract to the complete path).
	main := &scriptedProvider{id: "main", streamErr: []error{
		&provider.UpstreamError{HTTPStatus: 401, Message: "bad key"},
	}}
	fallback := &scriptedProvider{id: "fbm"}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, fallback),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "fbm", Model: "fbm"},
		},
	})

	_, _, trace, err := e.ExecuteStream(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, ir.Request{Model: "m", Stream: true})
	if err == nil {
		t.Fatal("a 401 stream chain must fail, not be rescued")
	}
	if fallback.calls != 0 {
		t.Error("fallback must not be consulted for non-gap stream failures")
	}
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventCapabilityFallback {
			t.Error("capability_fallback event recorded for a 401 stream chain")
		}
	}
}

func TestExecutor_CapabilityFallback_StreamFallbackAlsoGapFailsTerminal(t *testing.T) {
	// Chain and fallback model both reject the modality at dispatch: the
	// stream terminates with the last (typed) error — no recursion, no
	// infinite fallback chains.
	main := &scriptedProvider{id: "main", streamErr: []error{streamGapErr()}}
	fallback := &scriptedProvider{id: "fbm", streamErr: []error{streamGapErr()}}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, fallback),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "fbm", Model: "fbm"},
		},
	})

	_, _, _, err := e.ExecuteStream(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, ir.Request{Model: "m", Stream: true})
	if err == nil {
		t.Fatal("gap-exhausted fallback stream must fail")
	}
	var up *provider.UpstreamError
	if !errorsAs(err, &up) || up.HTTPStatus != 400 {
		t.Errorf("terminal error = %v, want the typed 400 gap error", err)
	}
}

// errorsAs is errors.As (local alias to keep the import surface minimal).
func errorsAs(err error, target **provider.UpstreamError) bool {
	for err != nil {
		if ue, ok := err.(*provider.UpstreamError); ok {
			*target = ue
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
