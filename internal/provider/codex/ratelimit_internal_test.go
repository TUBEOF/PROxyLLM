package codex

// The rate-limit header family the ChatGPT backend sends on every
// /codex/responses answer (codex-rs codex-api/src/rate_limits.rs,
// parse_rate_limit_for_limit with the default legacy limit id "codex",
// accessed 2026-10-23): x-codex-{primary,secondary}-{used-percent,
// window-minutes,reset-at}. The reset is a unix timestamp.

import (
	"net/http"
	"testing"
)

func TestRateLimitFromHeaders_ParsesBothWindows(t *testing.T) {
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "12.5")
	h.Set("x-codex-primary-window-minutes", "300")
	h.Set("x-codex-primary-reset-at", "1771000000")
	h.Set("x-codex-secondary-used-percent", "40")
	h.Set("x-codex-secondary-reset-at", "1772000000")

	rl, ok := rateLimitFromHeaders(h)
	if !ok {
		t.Fatal("headers carry a rate limit; ok must be true")
	}
	if rl.PrimaryUsedPct == nil || *rl.PrimaryUsedPct != 12.5 {
		t.Errorf("primary used = %v, want 12.5", rl.PrimaryUsedPct)
	}
	if rl.PrimaryResetAt != 1771000000 {
		t.Errorf("primary reset = %d, want 1771000000", rl.PrimaryResetAt)
	}
	if rl.SecondaryUsedPct == nil || *rl.SecondaryUsedPct != 40 {
		t.Errorf("secondary used = %v, want 40", rl.SecondaryUsedPct)
	}
	if rl.SecondaryResetAt != 1772000000 {
		t.Errorf("secondary reset = %d, want 1772000000", rl.SecondaryResetAt)
	}
}

// No rate-limit headers means no snapshot — a sink must not be called with
// zeros invented out of nothing.
func TestRateLimitFromHeaders_AbsentHeadersReportNothing(t *testing.T) {
	if _, ok := rateLimitFromHeaders(http.Header{}); ok {
		t.Error("empty headers must report no rate limit")
	}
}
