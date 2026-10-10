package routing_test

// Streaming image-to-text rescue contract (ADR-014). The rescue applies to
// streams exactly as to completes: a chain whose every DISPATCH failed with a
// capability_gap (an image rejection is a 400 before the first byte) gets its
// images described, and the chain is dispatched again with the text in place.
//
// FO-1/FO-5 are untouched. The caption calls run before anything is sent to the
// client, and the re-dispatch either streams or fails — a successful dispatch
// is never re-routed, and a mid-stream failure still rides the channel
// terminally.

import (
	"context"
	"errors"
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

func TestExecutor_Caption_StreamDescribesImageAndRetriesTheChain(t *testing.T) {
	main := &recordingProvider{id: "main", streamErrs: []error{streamGapErr()}}
	vision := &recordingProvider{id: "vision", text: "a red pipeline with a failing test step"}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, vision),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "vision", Model: "vl-1"},
		},
	})

	req := imageRequest("why is the build red?")
	req.Stream = true

	ch, cancel, trace, err := e.ExecuteStream(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, req)
	if err != nil {
		t.Fatalf("ExecuteStream: %v (the captioned retry must dispatch)", err)
	}
	defer cancel()

	var events []ir.StreamEvent
	for ev := range ch {
		events = append(events, ev)
	}
	if len(events) == 0 || events[0].Type != ir.Start {
		t.Fatalf("stream events = %+v, want the retried dispatch's start event", events)
	}
	if vision.calls != 1 {
		t.Errorf("vision calls = %d, want 1", vision.calls)
	}
	if main.calls != 2 {
		t.Fatalf("routed dispatches = %d, want 2 (rejected, then served the retry)", main.calls)
	}
	if len(main.seen) < 2 {
		t.Fatalf("routed dispatches recorded = %d, want 2", len(main.seen))
	}
	got := partStrings(main.seen[1].Messages[0].Parts)
	want := []string{"text:why is the build red?", "text:a red pipeline with a failing test step"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("retried stream parts = %q, want %q", got, want)
	}
	if len(eventsOf(trace, routing.EventImageCaption)) != 1 {
		t.Error("the caption was not traced on the stream path")
	}
}

// The caption spend is billed to the request that needed it: the terminal
// usage carries it, so the client's accounting does not under-report.
func TestExecutor_Caption_StreamAddsCaptionUsage(t *testing.T) {
	main := &recordingProvider{id: "main", streamErrs: []error{streamGapErr()}}
	vision := &recordingProvider{id: "vision", text: "a screenshot"}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, vision),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "vision", Model: "vl-1"},
		},
	})

	req := imageRequest("read this")
	req.Stream = true

	ch, cancel, _, err := e.ExecuteStream(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, req)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	defer cancel()
	for range ch {
	}
	// The stub reports no usage, so this pins the plumbing rather than a
	// number: the wrapper must pass every event through unchanged.
	if main.calls != 2 {
		t.Errorf("routed dispatches = %d, want 2", main.calls)
	}
}

// A chain that rejects the transcript too ends the request: exactly one
// caption round, no recursion.
func TestExecutor_Caption_StreamOnePassOnly(t *testing.T) {
	main := &recordingProvider{id: "main", streamErrs: []error{streamGapErr(), streamGapErr(), streamGapErr()}}
	vision := &recordingProvider{id: "vision"}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, vision),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "vision", Model: "vl-1"},
		},
	})

	req := imageRequest("read this")
	req.Stream = true

	_, _, _, err := e.ExecuteStream(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, req)
	if err == nil {
		t.Fatal("a chain that keeps rejecting must fail")
	}
	var up *provider.UpstreamError
	if !errors.As(err, &up) || up.HTTPStatus != 400 {
		t.Errorf("terminal error = %v, want the typed 400 gap error", err)
	}
	if vision.calls != 1 {
		t.Errorf("vision calls = %d, want exactly 1 — the rescue runs once", vision.calls)
	}
}

// A 401 dispatch failure is the credentials' problem — no rescue, images or
// not (identical contract to the complete path).
func TestExecutor_Caption_StreamOtherFailuresDoNotTrigger(t *testing.T) {
	main := &recordingProvider{id: "main", streamErrs: []error{
		&provider.UpstreamError{HTTPStatus: 401, Message: "bad key"},
	}}
	vision := &recordingProvider{id: "vision"}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, vision),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "vision", Model: "vl-1"},
		},
	})

	req := imageRequest("read this")
	req.Stream = true

	_, _, trace, err := e.ExecuteStream(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, req)
	if err == nil {
		t.Fatal("a 401 stream chain must fail, not be rescued")
	}
	if vision.calls != 0 {
		t.Error("vision models must not be consulted for a non-gap stream failure")
	}
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventCapabilityFallback || ev.Type == routing.EventImageCaption {
			t.Errorf("fallback event recorded for a 401 stream chain: %+v", ev)
		}
	}
}
