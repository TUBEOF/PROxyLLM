package routing

// Which failures are evidence about a provider, and which are not. The table
// is small on purpose: every class it excludes is one that used to open the
// breaker for the whole account, including the models the request never
// touched.

import (
	"net/http"
	"testing"
	"time"
)

func TestCountsTowardHealth_PerClass(t *testing.T) {
	cases := []struct {
		name   string
		class  Class
		status int
		want   bool
	}{
		// The provider answered correctly and said a model cannot do something.
		{"capability gap", ClassCapabilityGap, http.StatusBadRequest, false},
		{"context window", ClassContextWindow, http.StatusBadRequest, false},
		// The request or the caller is at fault.
		{"content filter", ClassContentFilter, http.StatusBadRequest, false},
		{"malformed request", ClassClientError, http.StatusBadRequest, false},
		{"not found", ClassClientError, http.StatusNotFound, false},
		// Nobody is waiting for the answer any more.
		{"client abort", ClassClientAbort, 0, false},
		// A quota rejection has its own, longer cooldown.
		{"quota exhausted", ClassInsufficientQuota, http.StatusTooManyRequests, false},
		// The account's credentials are an account fact.
		{"unauthorized", ClassClientError, http.StatusUnauthorized, true},
		{"forbidden", ClassClientError, http.StatusForbidden, true},
		// Real service observations.
		{"rate limited", ClassRateLimit, http.StatusTooManyRequests, true},
		{"server error", ClassServerError, http.StatusBadGateway, true},
		{"transport", ClassTransport, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := countsTowardHealth(Classification{Class: tc.class}, tc.status)
			if got != tc.want {
				t.Errorf("countsTowardHealth(%s, %d) = %v, want %v", tc.class, tc.status, got, tc.want)
			}
		})
	}
}

// The band below the breaker: an account that has been failing is worth seeing
// before it stops serving altogether, and the band must not gate anything.
func TestHealthTracker_DegradedBandPrecedesTheBreaker(t *testing.T) {
	h := NewHealthTracker()
	h.ReportFailure("p")
	if r := h.Reason("p"); r != "" {
		t.Errorf("after 1 of %d failures: %q, want no flag yet", h.Threshold, r)
	}
	for i := 1; i < h.Threshold-1; i++ {
		h.ReportFailure("p")
	}
	if r := h.Reason("p"); r != "degraded" {
		t.Errorf("one failure before the breaker: %q, want \"degraded\"", r)
	}
	if h.Cooldown("p") {
		t.Error("the degraded band must not take the account out of rotation")
	}
	// One more failure reaches the threshold, and a success clears everything.
	h.ReportFailure("p")
	if r := h.Reason("p"); r != "breaker" {
		t.Errorf("at the threshold: %q, want \"breaker\"", r)
	}
	if !h.Cooldown("p") {
		t.Error("at the threshold the account must be cooling")
	}
	h.ReportSuccess("p")
	if r := h.Reason("p"); r != "" {
		t.Errorf("after a success: %q, want healthy", r)
	}
}

// The kind of failure scales the cooldown; the escalation multiplies it. The
// second half is the load-bearing one: a kind-specific duration that replaced
// the escalation would re-probe a dead account at a fixed interval forever.
func TestHealthTracker_KindScalesTheCooldownAndEscalationStillApplies(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	h := NewHealthTracker()
	h.SetClock(func() time.Time { return now })

	// A rate limit opens for twice the base; a server error for the base.
	rl := NewHealthTracker()
	rl.SetClock(func() time.Time { return now })
	se := NewHealthTracker()
	se.SetClock(func() time.Time { return now })
	for i := 0; i < rl.Threshold; i++ {
		rl.ReportFailureKind("p", ClassRateLimit)
		se.ReportFailureKind("p", ClassServerError)
	}
	rateLimitUntil := rl.entries["p"].until
	serverUntil := se.entries["p"].until
	if rateLimitUntil.Equal(serverUntil) {
		t.Error("a rate limit and a server error produced the same cooldown")
	}
	if got := rateLimitUntil.Sub(serverUntil); got != h.BaseCooldown {
		t.Errorf("rate-limit cooldown exceeds the base by %v, want %v", got, h.BaseCooldown)
	}

	// A second opening cycle doubles whatever the kind produced — it does not
	// replace it.
	rl.ReportFailureKind("p", ClassRateLimit)
	secondOpen := rl.entries["p"].until
	if d := secondOpen.Sub(rateLimitUntil); d != rl.BaseCooldown*2 {
		t.Errorf("second open advanced the deadline by %v, want %v (the escalation multiplies the kind's cooldown)",
			d, rl.BaseCooldown*2)
	}
}
