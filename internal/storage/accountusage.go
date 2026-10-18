package storage

// ADR-012 — per-account usage snapshot (the Quota page's cached backend).
// The latest scrape result per account rides the config-KV under the
// `account_usage:<providerID>` namespace: one JSON blob, written by the
// usage prober after a live scrape and read by the admin API + WebUI. No new
// migration — the config table is the typed-free top layer; the typed shape
// lives here.

import (
	"context"
	"encoding/json"
	"fmt"
)

// AccountUsageBlob is one account's last-known usage/limit state. Percent
// values are 0–100 (used); reset times are unix seconds (0 = unknown).
type AccountUsageBlob struct {
	FetchedAt int64  // unix seconds; 0 = never fetched
	Error     string // last scrape error ("" = none)

	// Ollama-cloud live windows (percent used + reset, both from the
	// settings page).
	SessionPct *float64 `json:"session_pct,omitempty"`
	// SessionReset, WeeklyReset unix seconds (0 unknown).
	SessionReset int64    `json:"session_reset,omitempty"`
	WeeklyPct    *float64 `json:"weekly_pct,omitempty"`
	WeeklyReset  int64    `json:"weekly_reset,omitempty"`

	// Firecrawl team credits (the documented GET /v2/team/credit-usage
	// payload, re-read 2026-10-18): remaining/plan credits and the billing
	// period end. The Quota card shows remaining/plan as the ring and
	// "remaining credits left"; a missing value stays nil (honest unknown).
	RemainingCredits *float64 `json:"remaining_credits,omitempty"`
	PlanCredits      *float64 `json:"plan_credits,omitempty"`
	CreditsReset     int64    `json:"credits_reset,omitempty"`
}

// HasWindows reports whether the blob carries at least one live window —
// the honest "is there anything to draw" test. An error-only blob (a failed
// scrape persists Error and nothing else) has none, and the card renders
// that error instead of a pair of empty bars.
func (b AccountUsageBlob) HasWindows() bool {
	return b.SessionPct != nil || b.WeeklyPct != nil || b.SessionReset > 0 || b.WeeklyReset > 0 ||
		b.RemainingCredits != nil || b.PlanCredits != nil || b.CreditsReset > 0
}

// accountUsageKey is the config-KV key for one account's usage blob.
func accountUsageKey(providerID string) string {
	return "account_usage:" + providerID
}

// PutAccountUsage persists one account's usage blob. The blob was already
// shaped by the prober; encoding failures are storage-internal and surface.
func (s *Store) PutAccountUsage(ctx context.Context, providerID string, b AccountUsageBlob) error {
	raw, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("storage: encode account usage: %w", err)
	}
	return s.SetConfigValue(ctx, accountUsageKey(providerID), string(raw))
}

// GetAccountUsage reads one account's usage blob. Missing key → ok=false
// (the normal never-scraped state); corrupted blobs degrade to ok=false
// with a discarded payload (readers must never de-route on store noise —
// the fallbackList contract).
func (s *Store) GetAccountUsage(ctx context.Context, providerID string) (AccountUsageBlob, bool, error) {
	v, ok, err := s.GetConfigValue(ctx, accountUsageKey(providerID))
	if err != nil || !ok {
		return AccountUsageBlob{}, false, err
	}
	var b AccountUsageBlob
	if err := json.Unmarshal([]byte(v), &b); err != nil {
		return AccountUsageBlob{}, false, nil
	}
	return b, true, nil
}
