package storage

// ADR-012: account_usage KV round-trip.

import (
	"context"
	"path/filepath"
	"testing"
)

func TestAccountUsageBlob_RoundTrip(t *testing.T) {
	ctx := context.Background()
	st, err := Open(testCfg(filepath.Join(t.TempDir(), "test.db")))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()

	// Missing key: ok=false, zero blob, no error (the never-scraped state).
	if _, ok, err := st.GetAccountUsage(ctx, "acct-a"); err != nil || ok {
		t.Fatalf("missing: ok=%v err=%v, want false/nil", ok, err)
	}

	pct := 42.5
	want := AccountUsageBlob{
		FetchedAt:  1_790_000_000,
		SessionPct: &pct, SessionReset: 1_790_010_000,
		WeeklyPct: &pct, WeeklyReset: 1_790_020_000,
	}
	if err := st.PutAccountUsage(ctx, "acct-a", want); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok, err := st.GetAccountUsage(ctx, "acct-a")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.FetchedAt != want.FetchedAt ||
		got.SessionPct == nil || *got.SessionPct != pct || got.SessionReset != want.SessionReset ||
		got.WeeklyPct == nil || got.WeeklyReset != want.WeeklyReset {
		t.Fatalf("round trip mismatch: %+v", got)
	}

	// HasWindows is the "is there anything to draw" test: a blob with a
	// window draws, an error-only blob (the failed-scrape shape) does not —
	// the card renders the error instead of a pair of empty bars.
	if !got.HasWindows() {
		t.Error("a blob with session+weekly windows must report HasWindows")
	}
	if (AccountUsageBlob{Error: "HTTP 403"}).HasWindows() {
		t.Error("an error-only blob must not report HasWindows")
	}
	if (AccountUsageBlob{}).HasWindows() {
		t.Error("a zero blob must not report HasWindows")
	}

	// Corrupted blob degrades to ok=false (never an error).
	if err := st.SetConfigValue(ctx, accountUsageKey("acct-b"), "{not json"); err != nil {
		t.Fatalf("seed corrupt: %v", err)
	}
	if _, ok, err := st.GetAccountUsage(ctx, "acct-b"); err != nil || ok {
		t.Fatalf("corrupted: ok=%v err=%v, want false/nil", ok, err)
	}
}

// New blobs carry the uniform windows[] list; the legacy mirrors are
// dual-written so an older bundle still renders. Remaining percent is the
// UI-facing reading, used percent is the source-native one.
func TestAccountUsageBlob_WindowsAndLegacyMirrors(t *testing.T) {
	ctx := context.Background()
	st, err := Open(testCfg(filepath.Join(t.TempDir(), "test.db")))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()

	remaining := 71.2
	used := 28.8
	credits := 850.0
	limit := 1000.0
	blob := AccountUsageBlob{
		FetchedAt: 1_790_000_000,
		Windows: []UsageWindow{
			{Key: "session", Label: "Session", Kind: "percent",
				RemainingPct: &remaining, UsedPct: &used, ResetAt: 1_790_010_000,
				Source: "ollama_balance"},
			{Key: "credits", Label: "Credits", Kind: "credits", Unit: "credits",
				Remaining: &credits, Limit: &limit, ResetAt: 1_790_020_000,
				Source: "firecrawl_credit_usage"},
		},
	}
	if err := st.PutAccountUsage(ctx, "acct-a", blob); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok, err := st.GetAccountUsage(ctx, "acct-a")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.SchemaVersion != AccountUsageSchemaVersion {
		t.Errorf("schema = %d, want %d", got.SchemaVersion, AccountUsageSchemaVersion)
	}
	if got.SessionPct == nil || *got.SessionPct != used || got.SessionReset != 1_790_010_000 {
		t.Errorf("legacy session mirror = %v/%d, want %v/%d",
			got.SessionPct, got.SessionReset, used, 1_790_010_000)
	}
	if got.RemainingCredits == nil || *got.RemainingCredits != credits ||
		got.PlanCredits == nil || *got.PlanCredits != limit {
		t.Errorf("legacy credits mirror = %v/%v, want %v/%v",
			got.RemainingCredits, got.PlanCredits, credits, limit)
	}
	if len(got.Windows) != 2 || got.Windows[0].RemainingPct == nil ||
		*got.Windows[0].RemainingPct != remaining {
		t.Errorf("windows = %+v, want the two normalized entries", got.Windows)
	}
}

// Pre-v2 blobs use capitalized FetchedAt/Error and used-percent mirrors;
// they must decode without a migration and expose derived windows.
func TestAccountUsageBlob_LegacyJSONDecodes(t *testing.T) {
	ctx := context.Background()
	st, err := Open(testCfg(filepath.Join(t.TempDir(), "test.db")))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()

	legacy := `{"FetchedAt":1790000000,"Error":"","session_pct":28.8,` +
		`"session_reset":1790010000,"weekly_pct":27.4,"weekly_reset":1790020000}`
	if err := st.SetConfigValue(ctx, accountUsageKey("acct-legacy"), legacy); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, ok, err := st.GetAccountUsage(ctx, "acct-legacy")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.FetchedAt != 1_790_000_000 {
		t.Errorf("FetchedAt = %d, want the capitalized legacy value", got.FetchedAt)
	}
	if len(got.Windows) != 2 {
		t.Fatalf("windows = %+v, want session+weekly derived from legacy", got.Windows)
	}
	if got.Windows[0].RemainingPct == nil || *got.Windows[0].RemainingPct != 71.2 {
		t.Errorf("session remaining = %v, want 71.2", got.Windows[0].RemainingPct)
	}
	if got.Windows[0].UsedPct == nil || *got.Windows[0].UsedPct != 28.8 {
		t.Errorf("session used = %v, want 28.8", got.Windows[0].UsedPct)
	}
	if got.Windows[0].Source != "legacy" {
		t.Errorf("source = %q, want legacy", got.Windows[0].Source)
	}
}
