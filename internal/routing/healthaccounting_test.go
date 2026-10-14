package routing_test

// The health needle, at the level the operator sees it: whether the next
// request can still use the account.
//
// The regression this pins is the one the vision rescue walks straight into: a
// conversation with a screenshot produces a capability_gap per turn, and those
// used to count. Three of them opened the breaker for every model on the
// account — the text-only ones included, which had done nothing wrong.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
)

func TestExecutor_CapabilityGapsDoNotOpenTheBreaker(t *testing.T) {
	health := routing.NewHealthTracker()
	health.SetClock(func() time.Time { return time.Unix(1_700_000_000, 0) })
	// No capability fallbacks: the rescue cannot run, so every request ends on
	// the gap error — the state the operator was in before a vision model was
	// configured, and the one that took the account down.
	main := &recordingProvider{id: "main", errs: []error{gapErr(), gapErr(), gapErr(), gapErr(), nil}}
	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main),
		Health:       health,
		RetrySleeper: func(time.Duration) {},
	})
	chain := []routing.ChainEntry{{ProviderID: "main", Model: "m"}}

	for i := 0; i < 4; i++ {
		if _, _, err := e.Execute(context.Background(), chain, imageRequest("what does this error say?")); err == nil {
			t.Fatalf("request %d succeeded; the chain was supposed to reject the image", i+1)
		}
	}
	if reason := health.Reason("main"); reason != "" {
		t.Errorf("provider is cooling (%q) after four capability gaps: a model's capability is not the provider's health", reason)
	}

	// Control: the failure classes that DO say something about the provider
	// still open the breaker.
	down := &recordingProvider{id: "down", errs: []error{
		&provider.UpstreamError{HTTPStatus: http.StatusBadGateway, Message: "upstream exploded"},
		&provider.UpstreamError{HTTPStatus: http.StatusBadGateway, Message: "upstream exploded"},
		&provider.UpstreamError{HTTPStatus: http.StatusBadGateway, Message: "upstream exploded"},
	}}
	e2 := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(down),
		Health:       health,
		RetrySleeper: func(time.Duration) {},
	})
	for i := 0; i < 3; i++ {
		_, _, _ = e2.Execute(context.Background(), []routing.ChainEntry{{ProviderID: "down", Model: "m"}}, imageRequest("q"))
	}
	if health.Reason("down") == "" {
		t.Error("three 502s did not open the breaker; the health gate has stopped gating")
	}
}

// A request the client abandoned says nothing about the provider, and it is
// terminal: there is nobody left to hand a failover to.
func TestExecutor_ClientAbortIsNotAProviderFailure(t *testing.T) {
	health := routing.NewHealthTracker()
	main := &recordingProvider{id: "main", errs: []error{context.Canceled}}
	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(main),
		Health:       health,
		RetrySleeper: func(time.Duration) {},
	})

	_, trace, err := e.Execute(context.Background(), []routing.ChainEntry{{ProviderID: "main", Model: "m"}}, imageRequest("q"))
	if err == nil {
		t.Fatal("a cancelled attempt returned success")
	}
	if health.Reason("main") != "" {
		t.Errorf("provider is cooling (%q) after a cancellation: one abandoned request must not mark it unavailable", health.Reason("main"))
	}
	if main.calls != 1 {
		t.Errorf("attempts = %d, want 1: a cancellation is terminal, never a failover", main.calls)
	}
	failed := eventsOf(trace, routing.EventFailed)
	if len(failed) != 1 || failed[0].Class != routing.ClassClientAbort {
		t.Errorf("failure events = %+v, want one classified as client_abort", failed)
	}
	if hasEvent(trace, routing.EventFailover) {
		t.Error("a cancelled request failed over; the client is gone")
	}
}
