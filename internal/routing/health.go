package routing

import (
	"sync"
	"time"
)

// HealthTracker is the circuit-breaker state behind the health gate
// (plan Analysis E step 5: consecutive failures → cooldown, exponential
// backoff, probe recovery). Consecutive failures open the breaker for an
// exponentially growing cooldown; a successful request closes it and
// resets the backoff (probe recovery).
//
// The tracker is deliberately conservative and small: routing skips cooled
// providers rather than deprioritizing them (deterministic ordering), and
// recovery is passive — the next chain position that reaches the provider
// acts as the probe (no active health-check traffic against paid APIs).
type HealthTracker struct {
	mu      sync.RWMutex
	entries map[string]*healthEntry

	// Threshold is the consecutive-failure count that opens the breaker.
	Threshold int

	// BaseCooldown is the first cooldown duration; each subsequent open
	// doubles it up to MaxCooldown.
	BaseCooldown time.Duration

	// MaxCooldown caps the exponential backoff (FO-3 ceiling scaled from
	// its 2-4 s retry ceiling to a breaker-level pause).
	MaxCooldown time.Duration

	// now is the clock, injectable for tests (deterministic, no sleeps).
	now func() time.Time
}

type healthEntry struct {
	consecutive int
	opens       int
	until       time.Time

	// quotaUntil is the dedicated quota-exhaustion cooldown end: a class
	// insufficient_quota rejection cools the account directly (no
	// consecutive-failure preconditions — every request would just pay
	// the rejection again). Kept SEPARATE from the breaker fields so a
	// quota cooldown never inflates consecutive/backoff state, and vice
	// versa. Cleared by ReportSuccess like every other state.
	quotaUntil time.Time
}

// defaultQuotaReprobe is the cooldown when the upstream names no reset
// window: the account re-probes hourly (bounded, never a permanent death
// — a later success clears it, and the Quota page refresh updates it).
const defaultQuotaReprobe = time.Hour

// NewHealthTracker returns a tracker with the defaults derived from FO-3:
// open after 3 consecutive failures, first cooldown 5 s, doubling to a
// 5 min cap.
func NewHealthTracker() *HealthTracker {
	return &HealthTracker{
		entries:      make(map[string]*healthEntry),
		Threshold:    3,
		BaseCooldown: 5 * time.Second,
		MaxCooldown:  5 * time.Minute,
		now:          time.Now,
	}
}

// SetClock injects a clock (tests advance time deterministically instead
// of sleeping).
func (h *HealthTracker) SetClock(now func() time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = now
}

// clock returns the current clock (test-injectable).
func (h *HealthTracker) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// ReportFailure records one failed attempt for providerID. Reaching the
// threshold opens the breaker (or extends the cooldown of an already
// tripped breaker).
func (h *HealthTracker) ReportFailure(providerID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.entries[providerID]
	if e == nil {
		e = &healthEntry{}
		h.entries[providerID] = e
	}
	e.consecutive++
	if e.consecutive >= h.Threshold {
		e.opens++
		e.until = h.clock().Add(h.cooldown(e.opens))
	}
}

// ReportSuccess records one successful attempt: the breaker closes and the
// backoff resets (probe recovery). Quota-exhaustion state clears too —
// the account demonstrably serves again.
func (h *HealthTracker) ReportSuccess(providerID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.entries, providerID)
}

// ReportQuotaExhausted cools one account on a class insufficient_quota
// rejection (2026-10-06): until is the upstream-announced reset window
// when known (Retry-After / announced reset), else the zero time — which
// maps to the default hourly re-probe. The report never counts toward
// the breaker's consecutive failures.
func (h *HealthTracker) ReportQuotaExhausted(providerID string, until time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.entries[providerID]
	if e == nil {
		e = &healthEntry{}
		h.entries[providerID] = e
	}
	if until.After(h.clock()) {
		e.quotaUntil = until
	} else {
		e.quotaUntil = h.clock().Add(defaultQuotaReprobe)
	}
}

// Reason names the active cooldown for the Quota page badge
// ("quota_exhausted" | "breaker"; "" = healthy).
func (h *HealthTracker) Reason(providerID string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	e := h.entries[providerID]
	if e == nil || !h.cooling(e) {
		return ""
	}
	if h.clock().Before(e.quotaUntil) {
		return "quota_exhausted"
	}
	return "breaker"
}

// Cooldown reports whether providerID is currently in cooldown (breaker
// open or quota exhausted — the health gate skips it).
func (h *HealthTracker) Cooldown(providerID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	e := h.entries[providerID]
	if e == nil {
		return false
	}
	return h.cooling(e)
}

// cooling reports whether the entry blocks traffic under the current clock
// (caller holds the lock).
func (h *HealthTracker) cooling(e *healthEntry) bool {
	return h.clock().Before(e.until) || h.clock().Before(e.quotaUntil)
}

// cooldown computes the exponential backoff for the nth open, capped.
func (h *HealthTracker) cooldown(opens int) time.Duration {
	d := h.BaseCooldown
	for i := 1; i < opens; i++ {
		d *= 2
		if d >= h.MaxCooldown {
			return h.MaxCooldown
		}
	}
	return min(d, h.MaxCooldown)
}
