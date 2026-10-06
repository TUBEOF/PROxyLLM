package storage

// ADR-012 — per-account usage snapshot (the Quota page's cached backend).
// The latest scrape result per account rides the config-KV under the
// `account_usage:<providerID>` namespace: one JSON blob, written by the
// usage prober after a live scrape (or the local fallback counter) and
// read by the admin API + WebUI. No new migration — the config table is
// the typed-free top layer; the typed shape lives here.

import (
	"context"
	"encoding/json"
	"fmt"
)

// AccountUsageBlob is one account's last-known usage/limit state. Percent
// values are 0–100 (used); reset times are unix seconds (0 = unknown).
type AccountUsageBlob struct {
	FetchedAt int64  // unix seconds; 0 = never fetched
	Source    string // "ollama" (live scrape) | "local" (usage_daily) | "" (unknown)
	Plan      string // plan tier as the provider states it, "" unknown
	Error     string // last scrape error ("" = none)

	// Ollama-cloud live windows (percent used + reset, both from the
	// settings page).
	SessionPct *float64 `json:"session_pct,omitempty"`
	// SessionReset, WeeklyReset unix seconds (0 unknown).
	SessionReset int64    `json:"session_reset,omitempty"`
	WeeklyPct    *float64 `json:"weekly_pct,omitempty"`
	WeeklyReset  int64    `json:"weekly_reset,omitempty"`
	// MonthlyPct is the locally computed month window (no live source
	// today); MonthlyReset is the first day of the next month (unix).
	MonthlyPct   *float64 `json:"monthly_pct,omitempty"`
	MonthlyReset int64    `json:"monthly_reset,omitempty"`
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
