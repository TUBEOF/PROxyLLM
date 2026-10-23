package codex

// The ChatGPT backend reports an account's rate limits on the responses of
// live /codex/responses traffic — there is no usage endpoint to probe. The
// header family is codex-rs's own (codex-api/src/rate_limits.rs,
// parse_rate_limit_for_limit with the default legacy limit id "codex",
// accessed 2026-10-23):
//
//	x-codex-primary-used-percent      f64, percent of the window used
//	x-codex-primary-window-minutes    i64
//	x-codex-primary-reset-at          i64, unix seconds
//	x-codex-secondary-used-percent    f64 (the weekly window)
//	x-codex-secondary-window-minutes  i64
//	x-codex-secondary-reset-at        i64, unix seconds
//
// A snapshot exists only when the backend sent at least one used-percent —
// zeros are never invented.

import (
	"net/http"
	"strconv"
)

// RateLimits is one account's rate-limit state, read off a live response.
// Reset times are unix seconds; 0 = not reported.
type RateLimits struct {
	PrimaryUsedPct   *float64
	PrimaryResetAt   int64
	SecondaryUsedPct *float64
	SecondaryResetAt int64
}

// rateLimitFromHeaders parses the x-codex-* family. ok is false when no
// used-percent arrived — a response the backend did not meter (or a third
// party's error page) must not persist a fake snapshot.
func rateLimitFromHeaders(h http.Header) (RateLimits, bool) {
	var rl RateLimits
	if v, ok := headerF64(h, "x-codex-primary-used-percent"); ok {
		rl.PrimaryUsedPct = &v
	}
	if v, ok := headerF64(h, "x-codex-secondary-used-percent"); ok {
		rl.SecondaryUsedPct = &v
	}
	if rl.PrimaryUsedPct == nil && rl.SecondaryUsedPct == nil {
		return RateLimits{}, false
	}
	rl.PrimaryResetAt = headerI64(h, "x-codex-primary-reset-at")
	rl.SecondaryResetAt = headerI64(h, "x-codex-secondary-reset-at")
	return rl, true
}

func headerF64(h http.Header, name string) (float64, bool) {
	v := h.Get(name)
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

func headerI64(h http.Header, name string) int64 {
	v := h.Get(name)
	if v == "" {
		return 0
	}
	i, err := strconv.ParseInt(v, 10, 64)
	if err != nil || i < 0 {
		return 0
	}
	return i
}
