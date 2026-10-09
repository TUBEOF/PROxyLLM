package routing_test

// Capability-fallback context gate (FO-6 companion, 2026-10-09). The vision
// fallback re-sends the WHOLE conversation to a fallback model, so a
// fallback model whose context window is smaller than the request can never
// serve it — dispatching it wastes an attempt and, before FO-6, killed the
// walk outright (a context-length 400 was terminal, so the second fallback
// model was never tried and the client got a 400 it had not caused).
//
// Contract under test:
//  1. an entry whose declared window cannot hold the request is skipped
//     BEFORE dispatch, with a trace event (never silent), and the walk
//     continues to the next entry;
//  2. an unknown window (0) is never a reason to skip — the entry is
//     attempted and the FO-6 class fails it over if it really is too small;
//  3. when every entry is too small the request ends with the ORIGINAL
//     capability-gap error (the reason the fallback was consulted at all),
//     not a secondary or empty exhaustion.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// bigRequest builds a request whose estimated input clearly exceeds 128000
// tokens (600000 chars ≈ 150000 tokens at the estimator's 4 chars/token).
func bigRequest() ir.Request {
	return ir.Request{
		Model: "m",
		Messages: []ir.Message{{Parts: []ir.Part{
			ir.TextPart{Text: strings.Repeat("a", 600000)},
		}}},
	}
}

func TestExecutor_CapabilityFallback_SkipsTooSmallModel(t *testing.T) {
	main := &scriptedProvider{id: "main", completeErrs: []error{gapErr()}}
	small := &scriptedProvider{id: "small"}
	big := &scriptedProvider{id: "big"}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, small, big),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "small", Model: "small", MaxContext: 128000},
			{ProviderID: "big", Model: "big", MaxContext: 1000000},
		},
	})

	resp, trace, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, bigRequest())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.ID != "ok" {
		t.Errorf("fallback did not serve: %+v", resp)
	}
	if small.calls != 0 {
		t.Errorf("too-small fallback model was dispatched %d time(s), want 0", small.calls)
	}
	if big.calls == 0 {
		t.Error("the sufficiently large fallback model was never attempted")
	}

	sawGateSkip := false
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventSkipped && ev.ProviderID == "small" &&
			strings.Contains(ev.Reason, "context window too small") {
			sawGateSkip = true
		}
	}
	if !sawGateSkip {
		t.Error("the context-gate skip is not recorded in the trace (fallbacks must never be silent)")
	}
}

func TestExecutor_CapabilityFallback_UnknownWindowIsAttempted(t *testing.T) {
	// MaxContext 0 = undetected (a custom endpoint whose discovery reported
	// no window). Guessing would silently drop a working fallback; attempt
	// it and let FO-6 fail it over if it really is too small.
	main := &scriptedProvider{id: "main", completeErrs: []error{gapErr()}}
	unknown := &scriptedProvider{id: "unknown"}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, unknown),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "unknown", Model: "unknown"}, // MaxContext 0
		},
	})

	if _, _, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, bigRequest()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if unknown.calls == 0 {
		t.Error("a fallback model with an unknown context window must still be attempted")
	}
}

func TestExecutor_CapabilityFallback_AllTooSmallKeepsOriginalGapError(t *testing.T) {
	main := &scriptedProvider{id: "main", completeErrs: []error{gapErr()}}
	small := &scriptedProvider{id: "small"}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, small),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "small", Model: "small", MaxContext: 128000},
		},
	})

	_, _, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, bigRequest())
	if err == nil {
		t.Fatal("a request no fallback model can hold must fail")
	}
	if small.calls != 0 {
		t.Errorf("too-small fallback dispatched %d time(s), want 0", small.calls)
	}
	var up *provider.UpstreamError
	if !errors.As(err, &up) || up.HTTPStatus != 400 || !strings.Contains(up.Message, "image") {
		t.Errorf("terminal error = %v, want the ORIGINAL 400 capability-gap error (the reason the fallback was consulted at all)", err)
	}
}

// TestExecutor_CapabilityFallback_ContextOverflowContinuesTheWalk is the
// regression for the reported defect end to end: the first fallback model
// has an UNKNOWN window and answers with a real context-window 400; the
// walk must carry on to the second fallback model instead of dying there.
func TestExecutor_CapabilityFallback_ContextOverflowContinuesTheWalk(t *testing.T) {
	main := &scriptedProvider{id: "main", completeErrs: []error{gapErr()}}
	overflowing := &scriptedProvider{id: "overflowing", completeErrs: []error{
		&provider.UpstreamError{HTTPStatus: 400, Message: "prompt is too long: 400123 tokens > 200000 maximum"},
	}}
	big := &scriptedProvider{id: "big"}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, overflowing, big),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "overflowing", Model: "overflowing"},
			{ProviderID: "big", Model: "big"},
		},
	})

	resp, trace, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, bigRequest())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.ID != "ok" || big.calls == 0 {
		t.Errorf("a context-window rejection on the first fallback must fail over to the next (resp=%+v, big.calls=%d)", resp, big.calls)
	}

	sawClass := false
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventFailed && ev.ProviderID == "overflowing" &&
			ev.Class == routing.ClassContextWindow {
			sawClass = true
		}
	}
	if !sawClass {
		t.Error("the context-window classification of the fallback attempt is not recorded in the trace")
	}
}
