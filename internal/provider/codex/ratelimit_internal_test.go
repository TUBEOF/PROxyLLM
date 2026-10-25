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

// The full snapshot carries window lengths, extra limit families and the
// credit balance — the fields the Quota card needs for an honest cooldown.
func TestRateLimitFromHeaders_ParsesWindowsFamiliesAndCredits(t *testing.T) {
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "12.5")
	h.Set("x-codex-primary-window-minutes", "300")
	h.Set("x-codex-primary-reset-at", "1771000000")
	h.Set("x-codex-secondary-used-percent", "40")
	h.Set("x-codex-secondary-window-minutes", "10080")
	h.Set("x-codex-secondary-reset-at", "1772000000")
	h.Set("x-codex-credits-has-credits", "true")
	h.Set("x-codex-credits-unlimited", "false")
	h.Set("x-codex-credits-balance", "$1,234.50")
	h.Set("x-gpt-6-primary-used-percent", "55")
	h.Set("x-gpt-6-primary-window-minutes", "60")
	h.Set("x-gpt-6-primary-reset-at", "1773000000")
	h.Set("x-gpt-6-limit-name", "GPT-6")

	rl, ok := rateLimitFromHeaders(h)
	if !ok {
		t.Fatal("headers carry a rate limit; ok must be true")
	}
	if rl.Source != "codex_headers" {
		t.Errorf("source = %q, want codex_headers", rl.Source)
	}
	if rl.PrimaryWindowMinutes != 300 || rl.SecondaryWindowMinutes != 10080 {
		t.Errorf("window minutes = %d/%d, want 300/10080",
			rl.PrimaryWindowMinutes, rl.SecondaryWindowMinutes)
	}
	if len(rl.Families) != 2 {
		t.Fatalf("families = %+v, want the default + gpt-6", rl.Families)
	}
	if rl.Families[1].ID != "gpt-6" || rl.Families[1].Name != "GPT-6" {
		t.Errorf("extra family = %+v, want gpt-6/GPT-6", rl.Families[1])
	}
	if rl.Families[1].Primary == nil || rl.Families[1].Primary.UsedPct == nil ||
		*rl.Families[1].Primary.UsedPct != 55 {
		t.Errorf("extra primary = %+v, want 55%%", rl.Families[1].Primary)
	}
	if rl.Credits == nil || rl.Credits.Balance == nil || *rl.Credits.Balance != 1234.5 {
		t.Errorf("credits = %+v, want a parsed $1,234.50 balance", rl.Credits)
	}
	if !rl.Credits.HasCredits || rl.Credits.Unlimited {
		t.Errorf("credit flags = %+v, want has=true unlimited=false", rl.Credits)
	}
}

// A credit-only response is still a snapshot; a non-numeric balance stays
// raw rather than becoming a fabricated number.
func TestRateLimitFromHeaders_CreditsOnly(t *testing.T) {
	h := http.Header{}
	h.Set("x-codex-credits-has-credits", "true")
	h.Set("x-codex-credits-balance", "unlimited-ish")
	rl, ok := rateLimitFromHeaders(h)
	if !ok {
		t.Fatal("credit headers alone must produce a snapshot")
	}
	if rl.Credits == nil || rl.Credits.Balance != nil || rl.Credits.Raw != "unlimited-ish" {
		t.Errorf("credits = %+v, want raw text and no numeric balance", rl.Credits)
	}
}

// The SSE tap parses codex.rate_limits events from fragmented frames and
// ignores every other event.
func TestSSERateLimitTap_ParsesFragmentedEvent(t *testing.T) {
	var got []RateLimits
	tap := newSSERateLimitTap(func(rl RateLimits) { got = append(got, rl) })
	event := "event: codex.rate_limits\n" +
		`data: {"type":"codex.rate_limits","rate_limits":{` +
		`"primary":{"used_percent":12.5,"window_minutes":300,"reset_at":1771000000},` +
		`"secondary":{"used_percent":40,"window_minutes":10080,"reset_at":1772000000},` +
		`"credits":{"has_credits":true,"unlimited":false,"balance":"$12.50"},` +
		`"metered_limit_name":"codex"}}` + "\n\n" +
		"event: response.output_text.delta\n" + "data: {\"delta\":\"hi\"}\n\n"
	if _, err := tap.Write([]byte(event[:37])); err != nil {
		t.Fatal(err)
	}
	if _, err := tap.Write([]byte(event[37:])); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("sink calls = %d, want 1", len(got))
	}
	rl := got[0]
	if rl.Source != "codex_sse" || len(rl.Families) != 1 {
		t.Fatalf("snapshot = %+v, want one codex_sse family", rl)
	}
	if rl.PrimaryUsedPct == nil || *rl.PrimaryUsedPct != 12.5 ||
		rl.PrimaryWindowMinutes != 300 {
		t.Errorf("primary = %+v, want 12.5%% / 300m", rl.PrimaryUsedPct)
	}
	if rl.Credits == nil || rl.Credits.Balance == nil || *rl.Credits.Balance != 12.5 {
		t.Errorf("credits = %+v, want $12.50", rl.Credits)
	}
}
