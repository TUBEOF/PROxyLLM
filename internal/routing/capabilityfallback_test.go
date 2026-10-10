package routing_test

// Capability-gap learning (2026-09-21 automatic capabilities): every chain
// entry whose attempt failed with the capability_gap class is reported to the
// learning callback, carrying the FULL entry — provider ID plus the upstream
// wire model, the pair the storage layer needs to locate the model row. A
// failure of another class says nothing about capability and must not fire it.
//
// The rescue path is not part of this contract: since ADR-014 the vision models
// describe an image rather than serving the conversation, so a gap they report
// is not evidence about the routed model. The callback sees the rule chain.

import (
	"context"
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

func TestExecutor_OnCapabilityGap(t *testing.T) {
	main := &scriptedProvider{id: "main", completeErrs: []error{gapErr()}}

	var got []routing.ChainEntry
	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:        regOf(main),
		Health:          routing.NewHealthTracker(),
		RetrySleeper:    func(time.Duration) {},
		OnCapabilityGap: func(entry routing.ChainEntry) { got = append(got, entry) },
	})

	_, _, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "main", Model: "m"}}, imageRequest("read this"))
	if err == nil {
		t.Fatal("an all-gap chain must fail")
	}
	if len(got) != 1 || got[0].ProviderID != "main" || got[0].Model != "m" {
		t.Errorf("gap callbacks = %+v, want exactly the routed entry with its wire model", got)
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
