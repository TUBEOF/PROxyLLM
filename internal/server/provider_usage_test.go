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

func ptrFloat(v float64) *float64 { return &v }
