package routing_test

// Quota-exhaustion cooldown (2026-10-06 self-healing gap): the
// insufficient_quota class failed over but never marked the account —
// every subsequent request paid the rejection again until 3 CONSECUTIVE
// failures tripped the generic breaker. The contract: a quota exhaustion
// report puts THAT account into cooldown (no consecutive-failure
// preconditions), the health gate skips it, a known reset window sets the
// end, and a success clears it. The reason is observable for the Quota
// page badge.

import (
	"context"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

func TestHealthTracker_QuotaExhaustedCooldown(t *testing.T) {
	h := routing.NewHealthTracker()

	// Zero consecutive failures: quota alone must cool the account.
	h.ReportQuotaExhausted("acct", time.Time{})
	if !h.Cooldown("acct") {
		t.Fatal("quota-exhausted account must be in cooldown immediately")
	}
	if got := h.Reason("acct"); got != "quota_exhausted" {
		t.Fatalf("reason = %q, want quota_exhausted", got)
	}
	// Default re-probe window: bounded, not a permanent death (pinned by
	// the reset-window test below).
	// ReportSuccess clears quota state along with the breaker.
	h.ReportSuccess("acct")
	if h.Cooldown("acct") || h.Reason("acct") != "" {
		t.Fatal("success must clear quota-exhausted cooldown")
	}
}

func TestHealthTracker_QuotaResetWindow(t *testing.T) {
	h := routing.NewHealthTracker()
	base := time.Unix(1_700_000_000, 0)
	h.SetClock(func() time.Time { return base })

	// Known reset window (Retry-After / announced reset): cooldown ends there.
	reset := base.Add(2 * time.Hour)
	h.ReportQuotaExhausted("acct", reset)
	if !h.Cooldown("acct") {
		t.Fatal("must be cooling")
	}
	if h.Cooldown("") {
		// Sanity: unknown accounts are not cooling.
		t.Fatal("unknown account must not be cooling")
	}
	// After the reset window lapses, the account is eligible again.
	h.SetClock(func() time.Time { return reset.Add(time.Minute) })
	if h.Cooldown("acct") {
		t.Fatal("cooldown must lapse at the announced reset time")
	}
}

func TestHealthTracker_ReasonBreaker(t *testing.T) {
	h := routing.NewHealthTracker()
	for i := 0; i < h.Threshold; i++ {
		h.ReportFailure("acct")
	}
	if !h.Cooldown("acct") {
		t.Fatal("breaker must be open after consecutive failures")
	}
	if got := h.Reason("acct"); got != "breaker" {
		t.Fatalf("reason = %q, want breaker", got)
	}
}

// Executor contract: a quota rejection marks the account — the NEXT
// request skips it via the health gate without paying the rejection
// again (mock provider scripted 402; the routing-level gate is the
// HealthTracker.Cooldown consult in the resolver).
func TestExecutor_QuotaMarksAccount(t *testing.T) {
	p := &scriptedProvider{id: "p1", completeErrs: []error{
		&provider.UpstreamError{HTTPStatus: 402, Code: "insufficient_quota", Message: "quota"},
	}}
	h := routing.NewHealthTracker()
	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     regOf(p),
		Health:       h,
		RetrySleeper: func(time.Duration) {},
	})

	_, _, err := e.Execute(context.Background(),
		[]routing.ChainEntry{{ProviderID: "p1", Model: "m"}}, ir.Request{Model: "m"})
	if err == nil {
		t.Fatal("first request must fail (no other provider)")
	}
	if !h.Cooldown("p1") {
		t.Fatal("quota rejection must cool the account immediately")
	}
	if got := h.Reason("p1"); got != "quota_exhausted" {
		t.Fatalf("reason = %q, want quota_exhausted", got)
	}
}
