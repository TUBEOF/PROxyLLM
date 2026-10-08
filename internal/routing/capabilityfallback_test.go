package routing_test

// Capability-fallback executor contract tests (2026-09-21 automatic
// capabilities): when every chain entry failed with a capability_gap (the
// model cannot serve a required input modality), the executor consults the
// configured capability fallback models — additional chain entries resolved
// from the stored vision_fallback list. Every consultation is traced
// (capability_fallback + failover events). A chain that failed for OTHER
// reasons must NOT trigger the model fallback (a 401 is the request's
// fault; a different model would fail identically).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// gapErr is one upstream capability-gap rejection (400 + modality signal).
func gapErr() error {
	return &provider.UpstreamError{HTTPStatus: 400, Message: "image input is not supported by this model"}
}

func TestExecutor_CapabilityFallback_RescuesGapExhaustedChain(t *testing.T) {
	// Chain model: "m" (a vision-less primary); fallback model: "fbm".
	main := &scriptedProvider{id: "main", completeErrs: []error{gapErr()}}
	fallback := &scriptedProvider{id: "fbm"}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, fallback),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		// The fallback models: model IDs the httpapi layer resolved to
		// chain entries before Execute — here injected directly.
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "fbm", Model: "fbm"},
		},
	})

	resp, trace, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, ir.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.ID != "ok" {
		t.Errorf("fallback did not serve: %+v", resp)
	}
	if fallback.calls == 0 {
		t.Error("fallback model was never attempted")
	}
	// Never silent: the trace must show the capability_fallback consultation
	// and the failover to the fallback entry.
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

func TestExecutor_CapabilityFallback_OtherFailuresDoNotTrigger(t *testing.T) {
	// A 401 exhausted chain must NOT consult the fallback models —
	// credentials are the request's problem, a different model fails the
	// same way.
	main := &scriptedProvider{id: "main", completeErrs: []error{
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

	_, trace, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, ir.Request{Model: "m"})
	if err == nil {
		t.Fatal("a 401 chain must fail, not be rescued")
	}
	if fallback.calls != 0 {
		t.Error("fallback must not be consulted for non-gap failures")
	}
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventCapabilityFallback {
			t.Error("capability_fallback event recorded for a 401 chain")
		}
	}
}

func TestExecutor_CapabilityFallback_FallbackAlsoGapFailsTerminal(t *testing.T) {
	// Both the chain and every fallback model reject the modality: the
	// request terminates with the last (typed) error — no infinite
	// fallback chains.
	main := &scriptedProvider{id: "main", completeErrs: []error{gapErr()}}
	fallback := &scriptedProvider{id: "fbm", completeErrs: []error{gapErr()}}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, fallback),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "fbm", Model: "fbm"},
		},
	})

	_, _, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, ir.Request{Model: "m"})
	if err == nil {
		t.Fatal("exhausted fallback chain must fail")
	}
	var up *provider.UpstreamError
	if !errors.As(err, &up) || up.HTTPStatus != 400 {
		t.Errorf("terminal error = %v, want the typed 400 gap error", err)
	}
}

// TestExecutor_OnCapabilityGap: the learning callback fires once per chain
// entry whose attempt failed with the capability_gap class, carrying the
// FULL entry (provider + wire model — the pair the storage layer needs to
// locate the model row). Non-gap failures never fire it; the fallback
// models' own gaps do (they are chain entries of their walk).
func TestExecutor_OnCapabilityGap(t *testing.T) {
	main := &scriptedProvider{id: "main", completeErrs: []error{gapErr()}}
	fallback := &scriptedProvider{id: "fbm", completeErrs: []error{gapErr()}}

	var got []routing.ChainEntry
	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, fallback),
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "fbm", Model: "fbm"},
		},
		OnCapabilityGap: func(entry routing.ChainEntry) { got = append(got, entry) },
	})

	_, _, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, ir.Request{Model: "m"})
	if err == nil {
		t.Fatal("all-gap chain must fail")
	}
	if len(got) != 2 || got[0].ProviderID != "main" || got[0].Model != "m" ||
		got[1].ProviderID != "fbm" || got[1].Model != "fbm" {
		t.Errorf("gap callbacks = %+v, want the main and fallback entries with their wire models", got)
	}
}

// TestExecutor_OnCapabilityGap_NotOnOtherFailures: a 401 chain never fires
// the learning callback — credentials are not a capability statement.
func TestExecutor_OnCapabilityGap_NotOnOtherFailures(t *testing.T) {
	main := &scriptedProvider{id: "main", completeErrs: []error{
		&provider.UpstreamError{HTTPStatus: 401, Message: "bad key"},
	}}

	fired := false
	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:        regOf(main),
		Health:          routing.NewHealthTracker(),
		RetrySleeper:    func(time.Duration) {},
		OnCapabilityGap: func(routing.ChainEntry) { fired = true },
	})

	_, _, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, ir.Request{Model: "m"})
	if err == nil {
		t.Fatal("a 401 chain must fail")
	}
	if fired {
		t.Error("learning callback fired on a non-gap failure")
	}
}

func TestExecutor_CapabilityFallback_StaleEntryGetsNoEdge(t *testing.T) {
	// A fallback list can hold a since-deleted account (provider deletion
	// cascade-deletes the model rows; a stale live list is refreshed only
	// on the next fallback write — server.go ReloadProviders now re-resolves
	// it, this pins the walk's own defense). The failover edge must name an
	// account the walk actually ATTEMPTS, never the dead entry the walk
	// skips ("fallbacks that are never reached" is the defect the single-
	// edge shape exists to prevent).
	main := &scriptedProvider{id: "main", completeErrs: []error{gapErr()}}
	alive := &scriptedProvider{id: "alive"}

	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main, alive), // "stale" is NOT registered
		Health:       routing.NewHealthTracker(),
		RetrySleeper: func(time.Duration) {},
		CapabilityFallbacks: []routing.ChainEntry{
			{ProviderID: "stale", Model: "gone"}, // skipped: not registered
			{ProviderID: "alive", Model: "m2"},
		},
	})

	resp, trace, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, ir.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.ID != "ok" {
		t.Errorf("fallback did not serve: %+v", resp)
	}
	if alive.calls == 0 {
		t.Error("the reachable fallback was never attempted")
	}
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventFailover && ev.To == "stale" {
			t.Errorf("failover edge to the skipped stale account: %+v", ev)
		}
	}
	// The edge onto the account that actually served must still exist.
	sawEdge := false
	for _, ev := range trace.Events() {
		if ev.Type == routing.EventFailover && ev.To == "alive" {
			sawEdge = true
		}
	}
	if !sawEdge {
		t.Error("trace has no failover edge onto the attempted fallback 'alive'")
	}
}
