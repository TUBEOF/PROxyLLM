package server

// The codex-oauth usage wiring: live traffic is the account's only honest
// usage source (the ChatGPT backend has no usage endpoint), so the rate-limit
// snapshot parsed off response headers persists into the ADR-012 usage blob
// the Quota page reads.

import (
	"testing"

	"github.com/tubeof/proxyllm/internal/config"
	"github.com/tubeof/proxyllm/internal/provider/codex"
	"github.com/tubeof/proxyllm/internal/storage"
)

func TestPersistRateLimits_WritesTheUsageBlob(t *testing.T) {
	st, err := storage.Open(config.StorageConfig{Path: t.TempDir() + "/test.db"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	persistRateLimits(st, "acct-codex", codex.RateLimits{
		PrimaryUsedPct:   ptrFloat(25),
		PrimaryResetAt:   1771000000,
		SecondaryUsedPct: ptrFloat(75),
		SecondaryResetAt: 1772000000,
	})

	blob, ok, err := st.GetAccountUsage(t.Context(), "acct-codex")
	if err != nil || !ok {
		t.Fatalf("get usage: ok=%v err=%v", ok, err)
	}
	if blob.SessionPct == nil || *blob.SessionPct != 25 {
		t.Errorf("session pct = %v, want 25", blob.SessionPct)
	}
	if blob.SessionReset != 1771000000 {
		t.Errorf("session reset = %d, want 1771000000", blob.SessionReset)
	}
	if blob.WeeklyPct == nil || *blob.WeeklyPct != 75 {
		t.Errorf("weekly pct = %v, want 75", blob.WeeklyPct)
	}
	if blob.WeeklyReset != 1772000000 {
		t.Errorf("weekly reset = %d, want 1772000000", blob.WeeklyReset)
	}
	if blob.FetchedAt == 0 {
		t.Error("fetched_at must record when the snapshot arrived")
	}
}

func TestPersistRateLimits_WritesWindowsFamiliesAndCredits(t *testing.T) {
	st, err := storage.Open(config.StorageConfig{Path: t.TempDir() + "/test.db"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	persistRateLimits(st, "acct-codex", codex.RateLimits{
		Source: "codex_headers",
		Families: []codex.RateLimitFamily{
			{ID: "codex", Name: "Codex",
				Primary: &codex.RateLimitWindow{
					UsedPct: ptrFloat(12.5), WindowMinutes: 300, ResetAt: 1771000000,
				},
				Secondary: &codex.RateLimitWindow{
					UsedPct: ptrFloat(40), WindowMinutes: 10080, ResetAt: 1772000000,
				}},
			{ID: "gpt-6", Name: "GPT-6",
				Primary: &codex.RateLimitWindow{
					UsedPct: ptrFloat(55), WindowMinutes: 60, ResetAt: 1773000000,
				}},
		},
		Credits: &codex.RateLimitCredits{Present: true, Balance: ptrFloat(12.5)},
	})

	blob, ok, err := st.GetAccountUsage(t.Context(), "acct-codex")
	if err != nil || !ok {
		t.Fatalf("get usage: ok=%v err=%v", ok, err)
	}
	if len(blob.Windows) != 4 {
		t.Fatalf("windows = %+v, want primary/secondary/gpt-6/credits", blob.Windows)
	}
	primary := blob.Windows[0]
	if primary.Key != "codex.primary" || primary.Label != "5-hour" ||
		primary.RemainingPct == nil || *primary.RemainingPct != 87.5 ||
		primary.WindowSeconds != 300*60 || primary.Source != "codex_headers" {
		t.Errorf("primary window = %+v, want 5-hour / 87.5 remaining", primary)
	}
	if blob.Windows[1].Label != "Weekly" {
		t.Errorf("secondary label = %q, want Weekly", blob.Windows[1].Label)
	}
	if blob.Windows[2].Key != "codex.gpt-6.primary" || blob.Windows[2].Label != "GPT-6 · 1h" {
		t.Errorf("extra family = %+v, want GPT-6 · 1h", blob.Windows[2])
	}
	if blob.Windows[3].Key != "codex.credits" || blob.Windows[3].Remaining == nil ||
		*blob.Windows[3].Remaining != 12.5 {
		t.Errorf("credits window = %+v, want 12.5 credits", blob.Windows[3])
	}
	if blob.SessionPct == nil || *blob.SessionPct != 12.5 ||
		blob.WeeklyPct == nil || *blob.WeeklyPct != 40 {
		t.Errorf("legacy mirrors = %v/%v, want 12.5/40", blob.SessionPct, blob.WeeklyPct)
	}
}

func ptrFloat(v float64) *float64 { return &v }
