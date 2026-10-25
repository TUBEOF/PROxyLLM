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

// AccountUsageSchemaVersion is the current blob schema. Version 2 added the
// uniform windows[] list; the legacy session/weekly/credits mirrors remain
// readable and are dual-written for one release so a rollback binary still
// renders.
const AccountUsageSchemaVersion = 2

// UsageWindow is one limit window in the provider-neutral shape the Quota
// card renders. Percent fields are 0–100; RemainingPct is the UI-facing
// reading (the bar shows remaining), UsedPct is the source-native figure.
// ResetAt is unix seconds (0 = unknown).
type UsageWindow struct {
	Key           string   `json:"key"`                     // session, weekly, monthly, credits, codex.primary, ...
	Label         string   `json:"label,omitempty"`         // display label
	Kind          string   `json:"kind,omitempty"`          // percent | credits | usd
	RemainingPct  *float64 `json:"remaining_pct,omitempty"` // 0..100
	UsedPct       *float64 `json:"used_pct,omitempty"`      // source-native used percent
	Remaining     *float64 `json:"remaining,omitempty"`
	Limit         *float64 `json:"limit,omitempty"`
	Unit          string   `json:"unit,omitempty"` // credits | usd | requests
	Unlimited     bool     `json:"unlimited,omitempty"`
	ResetAt       int64    `json:"reset_at,omitempty"`       // unix seconds
	WindowSeconds int64    `json:"window_seconds,omitempty"` // 0 = unknown
	Source        string   `json:"source,omitempty"`         // ollama_balance | ollama_settings | firecrawl_credit_usage | codex_headers | codex_sse
	Note          string   `json:"note,omitempty"`           // non-numeric balance, plan notes
}

// AccountUsageBlob is one account's last-known usage/limit state.
type AccountUsageBlob struct {
	SchemaVersion int           `json:"schema_version,omitempty"`
	FetchedAt     int64         `json:"fetched_at,omitempty"` // unix seconds; 0 = never fetched
	Error         string        `json:"error,omitempty"`      // last scrape error ("" = none)
	Windows       []UsageWindow `json:"windows,omitempty"`

	// Legacy mirrors, kept for one release. SessionPct/WeeklyPct are USED
	// percent, matching the pre-v2 wire and the settings-page parser.
	SessionPct       *float64 `json:"session_pct,omitempty"`
	SessionReset     int64    `json:"session_reset,omitempty"`
	WeeklyPct        *float64 `json:"weekly_pct,omitempty"`
	WeeklyReset      int64    `json:"weekly_reset,omitempty"`
	RemainingCredits *float64 `json:"remaining_credits,omitempty"`
	PlanCredits      *float64 `json:"plan_credits,omitempty"`
	CreditsReset     int64    `json:"credits_reset,omitempty"`
}

// accountUsageBlobWire is the on-disk shape. It accepts and emits both the
// current snake_case keys and the pre-v2 capitalized "FetchedAt"/"Error"
// keys, because the original struct had no JSON tags.
type accountUsageBlobWire struct {
	SchemaVersion   int           `json:"schema_version,omitempty"`
	FetchedAt       int64         `json:"fetched_at,omitempty"`
	FetchedAtLegacy int64         `json:"FetchedAt,omitempty"`
	Error           string        `json:"error,omitempty"`
	ErrorLegacy     string        `json:"Error,omitempty"`
	Windows         []UsageWindow `json:"windows,omitempty"`

	SessionPct       *float64 `json:"session_pct,omitempty"`
	SessionReset     int64    `json:"session_reset,omitempty"`
	WeeklyPct        *float64 `json:"weekly_pct,omitempty"`
	WeeklyReset      int64    `json:"weekly_reset,omitempty"`
	RemainingCredits *float64 `json:"remaining_credits,omitempty"`
	PlanCredits      *float64 `json:"plan_credits,omitempty"`
	CreditsReset     int64    `json:"credits_reset,omitempty"`
}

// MarshalJSON emits both key spellings for one release.
func (b AccountUsageBlob) MarshalJSON() ([]byte, error) {
	return json.Marshal(accountUsageBlobWire{
		SchemaVersion: b.SchemaVersion,
		FetchedAt:     b.FetchedAt, FetchedAtLegacy: b.FetchedAt,
		Error: b.Error, ErrorLegacy: b.Error,
		Windows:          b.Windows,
		SessionPct:       b.SessionPct,
		SessionReset:     b.SessionReset,
		WeeklyPct:        b.WeeklyPct,
		WeeklyReset:      b.WeeklyReset,
		RemainingCredits: b.RemainingCredits,
		PlanCredits:      b.PlanCredits,
		CreditsReset:     b.CreditsReset,
	})
}

// UnmarshalJSON accepts both key spellings.
func (b *AccountUsageBlob) UnmarshalJSON(raw []byte) error {
	var w accountUsageBlobWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return err
	}
	*b = AccountUsageBlob{
		SchemaVersion:    w.SchemaVersion,
		FetchedAt:        w.FetchedAt,
		Error:            w.Error,
		Windows:          w.Windows,
		SessionPct:       w.SessionPct,
		SessionReset:     w.SessionReset,
		WeeklyPct:        w.WeeklyPct,
		WeeklyReset:      w.WeeklyReset,
		RemainingCredits: w.RemainingCredits,
		PlanCredits:      w.PlanCredits,
		CreditsReset:     w.CreditsReset,
	}
	if b.FetchedAt == 0 {
		b.FetchedAt = w.FetchedAtLegacy
	}
	if b.Error == "" {
		b.Error = w.ErrorLegacy
	}
	return nil
}

// Normalize derives windows from the legacy mirrors (or vice versa) and
// stamps the schema version. It is called on every read and write.
func (b *AccountUsageBlob) Normalize() {
	if len(b.Windows) == 0 {
		b.Windows = b.windowsFromLegacy()
	}
	if len(b.Windows) > 0 {
		b.applyLegacyMirrors()
	}
	if b.SchemaVersion == 0 {
		b.SchemaVersion = AccountUsageSchemaVersion
	}
}

func (b *AccountUsageBlob) windowsFromLegacy() []UsageWindow {
	var out []UsageWindow
	if b.SessionPct != nil || b.SessionReset > 0 {
		w := UsageWindow{Key: "session", Label: "Session", Kind: "percent",
			ResetAt: b.SessionReset, Source: "legacy"}
		if b.SessionPct != nil {
			used := clampPct(*b.SessionPct)
			w.UsedPct = &used
			remaining := 100 - used
			w.RemainingPct = &remaining
		}
		out = append(out, w)
	}
	if b.WeeklyPct != nil || b.WeeklyReset > 0 {
		w := UsageWindow{Key: "weekly", Label: "Weekly", Kind: "percent",
			ResetAt: b.WeeklyReset, Source: "legacy"}
		if b.WeeklyPct != nil {
			used := clampPct(*b.WeeklyPct)
			w.UsedPct = &used
			remaining := 100 - used
			w.RemainingPct = &remaining
		}
		out = append(out, w)
	}
	if b.RemainingCredits != nil || b.PlanCredits != nil || b.CreditsReset > 0 {
		out = append(out, UsageWindow{
			Key: "credits", Label: "Credits", Kind: "credits", Unit: "credits",
			Remaining: b.RemainingCredits, Limit: b.PlanCredits,
			ResetAt: b.CreditsReset, Source: "legacy",
		})
	}
	return out
}

func (b *AccountUsageBlob) applyLegacyMirrors() {
	for _, w := range b.Windows {
		switch w.Key {
		case "session":
			if w.UsedPct != nil {
				used := clampPct(*w.UsedPct)
				b.SessionPct = &used
			} else if w.RemainingPct != nil {
				used := 100 - clampPct(*w.RemainingPct)
				b.SessionPct = &used
			}
			b.SessionReset = w.ResetAt
		case "weekly":
			if w.UsedPct != nil {
				used := clampPct(*w.UsedPct)
				b.WeeklyPct = &used
			} else if w.RemainingPct != nil {
				used := 100 - clampPct(*w.RemainingPct)
				b.WeeklyPct = &used
			}
			b.WeeklyReset = w.ResetAt
		case "credits":
			b.RemainingCredits = w.Remaining
			b.PlanCredits = w.Limit
			b.CreditsReset = w.ResetAt
		}
	}
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// HasWindows reports whether the blob carries at least one live window —
// the honest "is there anything to draw" test. An error-only blob (a failed
// scrape persists Error and nothing else) has none, and the card renders
// that error instead of a pair of empty bars.
func (b AccountUsageBlob) HasWindows() bool {
	return len(b.Windows) > 0 ||
		b.SessionPct != nil || b.WeeklyPct != nil || b.SessionReset > 0 || b.WeeklyReset > 0 ||
		b.RemainingCredits != nil || b.PlanCredits != nil || b.CreditsReset > 0
}

// accountUsageKey is the config-KV key for one account's usage blob.
func accountUsageKey(providerID string) string {
	return "account_usage:" + providerID
}

// PutAccountUsage persists one account's usage blob. The blob was already
// shaped by the prober; encoding failures are storage-internal and surface.
func (s *Store) PutAccountUsage(ctx context.Context, providerID string, b AccountUsageBlob) error {
	b.Normalize()
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
	b.Normalize()
	return b, true, nil
}
