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
