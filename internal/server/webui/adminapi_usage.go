// Admin usage endpoints (ADR-012, amendment 2026-10-17): the Quota page's
// backend — cached account usage state and on-demand refresh against the
// ollama.com settings page. The card shows the account's own limits only;
// the local usage_daily aggregate no longer feeds it.
package webui

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/tubeof/proxyllm/internal/storage"
)

// accountUsageDTO is one account's Quota-page card data. Percent values
// are pointers (nil = unknown, the UI renders "—").
type accountUsageDTO struct {
	AccountID string `json:"account_id"`
	Type      string `json:"type"`
	TypeLabel string `json:"type_label,omitempty"`
	Kind      string `json:"kind,omitempty"`
	TypeOrder int    `json:"type_order,omitempty"`
	Name      string `json:"name"`

	// Enabled is the account's pool switch: a disabled account serves no
	// traffic, and the card says so rather than showing a cooldown it is not
	// in a position to earn.
	Enabled bool `json:"enabled"`

	// Live is the account's own limit state when the last scrape produced a
	// window — nil renders the card's empty state (with Error when the scrape
	// failed). A blob with no window is not a "live" reading.
	Live  *liveUsageDTO `json:"live,omitempty"`
	Error string        `json:"error,omitempty"`

	// Health snapshot (breaker/quota cooldown). Empty = healthy.
	Health      string       `json:"health,omitempty"`
	HealthUntil int64        `json:"health_until,omitempty"`
	Cooldown    *cooldownDTO `json:"cooldown,omitempty"`

	FetchedAt int64 `json:"fetched_at,omitempty"`

	// Refreshable says the production prober can scrape this account type.
	// The UI uses it instead of hard-coding provider type strings, so the
	// "Refresh now"/"Refresh all" affordances stay in step with the prober.
	Refreshable bool `json:"refreshable,omitempty"`
}

// usageWindowDTO is the provider-neutral limit window the Quota card renders.
type usageWindowDTO struct {
	Key           string   `json:"key"`
	Label         string   `json:"label,omitempty"`
	Kind          string   `json:"kind,omitempty"`
	RemainingPct  *float64 `json:"remaining_pct,omitempty"`
	UsedPct       *float64 `json:"used_pct,omitempty"`
	Remaining     *float64 `json:"remaining,omitempty"`
	Limit         *float64 `json:"limit,omitempty"`
	Unit          string   `json:"unit,omitempty"`
	Unlimited     bool     `json:"unlimited,omitempty"`
	ResetAt       int64    `json:"reset_at,omitempty"`
	WindowSeconds int64    `json:"window_seconds,omitempty"`
	Source        string   `json:"source,omitempty"`
	Note          string   `json:"note,omitempty"`
}

// cooldownDTO carries the active cooldown deadlines (unix seconds; 0 =
// inactive). The badge reason is also mirrored in the legacy health field.
type cooldownDTO struct {
	Reason       string `json:"reason"`
	Until        int64  `json:"until,omitempty"`
	QuotaUntil   int64  `json:"quota_until,omitempty"`
	BreakerUntil int64  `json:"breaker_until,omitempty"`
}

type liveUsageDTO struct {
	// Windows is the current, provider-neutral reading.
	Windows []usageWindowDTO `json:"windows,omitempty"`

	// Legacy mirrors; kept for one release so an older bundle still renders.
	SessionPct   *float64 `json:"session_pct,omitempty"`
	SessionReset int64    `json:"session_reset,omitempty"`
	WeeklyPct    *float64 `json:"weekly_pct,omitempty"`
	WeeklyReset  int64    `json:"weekly_reset,omitempty"`

	// Credits is the Firecrawl team-credit window: remaining/plan and the
	// billing-period end. Pointers keep an unknown count unknown.
	Credits *creditsUsageDTO `json:"credits,omitempty"`
}

type creditsUsageDTO struct {
	Remaining *float64 `json:"remaining,omitempty"`
	Limit     *float64 `json:"limit,omitempty"`
	Reset     int64    `json:"reset,omitempty"`
}

// UsageProbeType reports whether the production prober implements a scrape
// for the provider type. The Quota list, the refresh affordances and the
// server's prober all read this, so a new probe is one entry, not a UI
// change.
func UsageProbeType(providerType string) bool {
	switch providerType {
	case "ollama-cloud", "ollama-cloud-web", "firecrawl":
		return true
	default:
		return false
	}
}

// getAccountUsage serves GET /api/admin/accounts/usage — one entry per
// non-web_tool provider account (the card list).
func (a *AdminAPI) getAccountUsage(w http.ResponseWriter, r *http.Request) {
	accts, err := a.store.ListProviders(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	out := make([]accountUsageDTO, 0, len(accts))
	for _, acct := range accts {
		if desc, isType := providerTypeByID(acct.Type); isType && desc.Kind == KindWebTool && !UsageProbeType(acct.Type) {
			continue // web-tool accounts without a usage probe have no card
		}
		entry := accountUsageDTO{
			AccountID:   acct.ID,
			Type:        acct.Type,
			Name:        acct.Name,
			Enabled:     acct.Enabled,
			Refreshable: UsageProbeType(acct.Type),
		}
		if desc, ok := providerTypeByID(acct.Type); ok {
			entry.TypeLabel = desc.Label
			entry.Kind = desc.Kind
			entry.TypeOrder = providerTypeOrder(acct.Type)
		}
		if blob, ok, err := a.store.GetAccountUsage(r.Context(), acct.ID); err == nil && ok {
			entry.FetchedAt = blob.FetchedAt
			entry.Error = blob.Error
			// Only a blob that carries a window is a live reading: an
			// error-only blob must render the empty state (with its error),
			// never a pair of empty bars.
			if blob.HasWindows() {
				entry.Live = &liveUsageDTO{
					SessionPct:   blob.SessionPct,
					SessionReset: blob.SessionReset,
					WeeklyPct:    blob.WeeklyPct,
					WeeklyReset:  blob.WeeklyReset,
				}
				for _, w := range blob.Windows {
					entry.Live.Windows = append(entry.Live.Windows, usageWindowDTO{
						Key: w.Key, Label: w.Label, Kind: w.Kind,
						RemainingPct: w.RemainingPct, UsedPct: w.UsedPct,
						Remaining: w.Remaining, Limit: w.Limit,
						Unit: w.Unit, Unlimited: w.Unlimited,
						ResetAt: w.ResetAt, WindowSeconds: w.WindowSeconds,
						Source: w.Source, Note: w.Note,
					})
				}
				if blob.RemainingCredits != nil || blob.PlanCredits != nil || blob.CreditsReset > 0 {
					entry.Live.Credits = &creditsUsageDTO{
						Remaining: blob.RemainingCredits,
						Limit:     blob.PlanCredits,
						Reset:     blob.CreditsReset,
					}
				}
			}
		}
		if a.healthSnapshot != nil {
			hv := a.healthSnapshot(acct.ID)
			entry.Health = hv.Reason
			entry.HealthUntil = max(hv.QuotaUntil, hv.BreakerUntil)
			if hv.Reason != "" || hv.QuotaUntil > 0 || hv.BreakerUntil > 0 {
				entry.Cooldown = &cooldownDTO{
					Reason: hv.Reason, Until: entry.HealthUntil,
					QuotaUntil: hv.QuotaUntil, BreakerUntil: hv.BreakerUntil,
				}
			}
		}
		out = append(out, entry)
	}
	// Stable order: by account id.
	sort.Slice(out, func(i, j int) bool { return out[i].AccountID < out[j].AccountID })
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// UsageCookieTokenKind is the encrypted token-store kind holding an
// account's ollama.com __Secure-session value (ADR-012). The server
// package's prober reads the same kind.
const UsageCookieTokenKind = "usage_cookie"

// UsageProbeFunc scrapes one account NOW and persists the blob. Returns
// ok=false + the HTTP-mapped status/text on failure. Production wires
// the ollama.com settings scrape (server.prodUsageProber); tests stub it.
type UsageProbeFunc func(ctx context.Context, accountID string, store *storage.Store) RefreshOutcome

// RefreshOutcome is one account-refresh result; the endpoints map it to
// the response status.
type RefreshOutcome struct {
	OK     bool
	Status int
	Text   string
	// Skipped means the prober's throttle served the cached reading instead
	// of calling upstream; RetryAfter is the wait in seconds.
	Skipped    bool
	RetryAfter int64
}

// accountsUsageRefresh handles POST /api/admin/accounts/usage/refresh/{id}
// — scrape-now for ONE account (only ollama-cloud accounts with a usage
// cookie; everything else answers 404/400).
func (a *AdminAPI) accountsUsageRefresh(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing account id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	out := a.refreshAccountUsage(ctx, id)
	if out.OK {
		body := map[string]any{"ok": true}
		if out.Skipped {
			body["skipped"] = true
			body["retry_after"] = out.RetryAfter
		}
		writeJSON(w, http.StatusOK, body)
		return
	}
	writeError(w, out.Status, out.Text)
}

// refreshAccountUsage invokes the wired probe func (production: the
// fixed-host ollama.com scrape; tests stub it).
func (a *AdminAPI) refreshAccountUsage(ctx context.Context, accountID string) RefreshOutcome {
	return a.prober(ctx, accountID, a.store)
}

// overviewDTO is the Dashboard's single-poll snapshot (live
// updates): the trailing 14 UTC days of usage plus today's aggregate —
// one GET replaces the Overview page's multi-request mount and makes the
// poll trivial.
type overviewDTO struct {
	GeneratedAt int64            `json:"generated_at"`
	Usage       []usageDTO       `json:"usage"`
	Today       overviewTodayDTO `json:"today"`
}

type overviewTodayDTO struct {
	Requests  int64 `json:"requests"`
	Failures  int64 `json:"failures"`
	InputTok  int64 `json:"input_tokens"`
	OutputTok int64 `json:"output_tokens"`
}

// overviewWindowDays is the snapshot's trailing window.
const overviewWindowDays = 14

// getOverview serves GET /api/admin/overview — the assembled dashboard
// snapshot on the injected clock (window anchoring is deterministic for
// the fixturesrv).
func (a *AdminAPI) getOverview(w http.ResponseWriter, r *http.Request) {
	today := time.Unix(a.now(), 0).UTC()
	to := today.Format("2006-01-02")
	from := today.AddDate(0, 0, -(overviewWindowDays - 1)).Format("2006-01-02")
	rows, err := a.store.QueryUsageDaily(r.Context(), from, to)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// No name projection here on purpose: same rationale as getUsage —
	// getOverview aggregates the rows; no account or key cell is rendered
	// from them, so the fields would be response weight and the
	// projection's two config-table reads per poll pure waste.
	usage := make([]usageDTO, 0, len(rows))
	var td overviewTodayDTO
	todayStr := to
	for _, u := range rows {
		usage = append(usage, usageDTO{
			Day:             u.Day,
			ClientKeyID:     u.ClientKeyID,
			ProviderID:      u.ProviderID,
			Model:           u.Model,
			InputTokens:     u.InputTokens,
			OutputTokens:    u.OutputTokens,
			CacheReadTokens: u.CacheReadTokens,
			Requests:        u.Requests,
			Failures:        u.Failures,
		})
		if u.Day == todayStr {
			td.Requests += u.Requests
			td.Failures += u.Failures
			td.InputTok += u.InputTokens
			td.OutputTok += u.OutputTokens
		}
	}
	writeJSON(w, http.StatusOK, overviewDTO{
		GeneratedAt: a.now(),
		Usage:       usage,
		Today:       td,
	})
}
