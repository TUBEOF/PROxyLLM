package webui

// Admin usage endpoints (ADR-012, amendment 2026-10-17) — contract tests via
// the standard adminenv harness. The Quota page's GET lists every non-web_tool
// account with its own limit state (the cached scrape blob) and the health
// snapshot; the refresh endpoint scrapes ONE account on demand (prober
// stubbed; production wires the fixed ollama.com client).

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/storage"
)

// newUsageEnv is a newAdminEnv with the ADR-012 wiring: a fixed clock and
// a stubbed prober recording whose blob it was asked to write.
func newUsageEnv(t *testing.T) (*adminEnv, *[]string) {
	e := newAdminEnv(t)
	probed := []string{}
	p := &probed
	e.api.prober = func(ctx context.Context, accountID string, store *storage.Store) RefreshOutcome {
		*p = append(*p, accountID)
		pct := 12.5
		if err := store.PutAccountUsage(ctx, accountID, storage.AccountUsageBlob{
			FetchedAt: 1_790_000_000, SessionPct: &pct,
		}); err != nil {
			return RefreshOutcome{Status: http.StatusInternalServerError, Text: "persist failed"}
		}
		return RefreshOutcome{OK: true}
	}
	return e, p
}

func TestAccountUsage_ListsAccountsAndWindows(t *testing.T) {
	e, _ := newUsageEnv(t)

	// Seed usage against the test clock BEFORE the admin session exists
	// (the env's Now closure reads e.now live: changing it after
	// bootstrap would expire the session's idle window).
	e.now = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).Unix()

	s := e.bootstrap(t, "admin")
	ctx := context.Background()
	if err := e.api.store.EnsureProvider(ctx, "acc1", "ollama-cloud", "Acc 1"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	// A live blob (cached scrape).
	pct := 33.3
	if err := e.api.store.PutAccountUsage(ctx, "acc1", storage.AccountUsageBlob{
		FetchedAt:  1_790_000_001,
		SessionPct: &pct, SessionReset: 1_790_010_000,
	}); err != nil {
		t.Fatalf("blob: %v", err)
	}

	w := e.authedCall(t, http.MethodGet, "/api/admin/accounts/usage", "", s)
	assertStatus(t, w, http.StatusOK, "accounts usage")
	var got struct {
		Accounts []accountUsageDTO `json:"accounts"`
	}
	decodeJSON(t, w, &got)
	if len(got.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(got.Accounts))
	}
	a := got.Accounts[0]
	if a.AccountID != "acc1" || a.Type != "ollama-cloud" || a.Name != "Acc 1" {
		t.Fatalf("account = %+v", a)
	}
	if a.Live == nil || a.Live.SessionPct == nil || *a.Live.SessionPct != 33.3 {
		t.Fatalf("live = %+v", a.Live)
	}
	if a.FetchedAt != 1_790_000_001 {
		t.Fatalf("fetched_at = %d", a.FetchedAt)
	}
}

// TestAccountUsage_ErrorBlobRendersNoLiveWindows pins the honest-empty path
// (ADR-012 amendment): a failed scrape persists an error-only blob, and the
// card must render that error — never "live": {} under two empty bars, which
// is what the old unconditional Live assignment produced. Seeded local usage
// rows exist on purpose: they must not surface anywhere on the card, which is
// exactly what "only the account's limit" means.
func TestAccountUsage_ErrorBlobRendersNoLiveWindows(t *testing.T) {
	e, _ := newUsageEnv(t)
	s := e.bootstrap(t, "admin")
	ctx := context.Background()
	if err := e.api.store.EnsureProvider(ctx, "acc1", "ollama-cloud", "Acc 1"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := e.api.store.IncrUsageDaily(ctx, storage.UsageDaily{
		Day: "2026-10-01", ProviderID: "acc1", Model: "glm",
		Requests: 7, Failures: 2, InputTokens: 111, OutputTokens: 22,
	}); err != nil {
		t.Fatalf("usage: %v", err)
	}
	if err := e.api.store.PutAccountUsage(ctx, "acc1", storage.AccountUsageBlob{
		FetchedAt: 1_790_000_002, Error: "HTTP 403 from the settings page",
	}); err != nil {
		t.Fatalf("blob: %v", err)
	}

	w := e.authedCall(t, http.MethodGet, "/api/admin/accounts/usage", "", s)
	assertStatus(t, w, http.StatusOK, "accounts usage")
	var raw struct {
		Accounts []map[string]any `json:"accounts"`
	}
	decodeJSON(t, w, &raw)
	if len(raw.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(raw.Accounts))
	}
	card := raw.Accounts[0]
	if live, ok := card["live"]; ok {
		t.Errorf("an error-only blob must not render a live reading, got %v", live)
	}
	if card["error"] == nil || card["error"] == "" {
		t.Errorf("the scrape error must ride the card: %v", card)
	}
	if local, ok := card["local"]; ok {
		t.Errorf("the local month aggregate is retired; the card still carries it: %v", local)
	}
	if source, ok := card["source"]; ok {
		t.Errorf("the source field is retired; the card still carries it: %v", source)
	}
}

func TestAccountUsage_RequiresSession(t *testing.T) {
	e, _ := newUsageEnv(t)
	e.bootstrap(t, "admin")
	w := e.call(t, http.MethodGet, "/api/admin/accounts/usage", "", "")
	assertStatus(t, w, http.StatusUnauthorized, "no session")
}

func TestAccountUsage_RefreshRunsProber(t *testing.T) {
	e, probed := newUsageEnv(t)
	s := e.bootstrap(t, "admin")
	ctx := context.Background()
	if err := e.api.store.EnsureProvider(ctx, "acc1", "ollama-cloud", "Acc 1"); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	w := e.authedCall(t, http.MethodPost, "/api/admin/accounts/usage/refresh/acc1", "", s)
	assertStatus(t, w, http.StatusOK, "refresh")
	if len(*probed) != 1 || (*probed)[0] != "acc1" {
		t.Fatalf("probed = %v, want [acc1]", *probed)
	}
}

func TestAccountUsage_RefreshUnknownAccountIs404(t *testing.T) {
	e, probed := newUsageEnv(t)
	s := e.bootstrap(t, "admin")
	// The stub prober accepts everything; the unstubbed production prober
	// would answer unknown-account — pin the ENDPOINT's pass-through only.
	w := e.authedCall(t, http.MethodPost, "/api/admin/accounts/usage/refresh/no-such", "", s)
	assertStatus(t, w, http.StatusOK, "refresh unknown (stub accepts)")
	if len(*probed) != 1 {
		t.Fatal("prober must be invoked with the path id")
	}
}
