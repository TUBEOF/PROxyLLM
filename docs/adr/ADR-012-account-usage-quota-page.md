# ADR-012: Per-account usage snapshot (Quota page)

- **Status**: Accepted
- **Date**: 2026-10-06
- **Amends**: ADR-010 (capability fallbacks); complements ADR-009 (accounts/strategies)

## Context

The Quota page needs per-account usage/limit state. ollama.com has no quota
API, so the only source for live usage is its settings page: scraping
`https://ollama.com/settings` with the account's browser session cookie
(`__Secure-session`) and parsing the `data-usage-track` blocks (session %,
weekly %, reset timestamps, plan tier). Chat-usage accounting already exists
(`usage_daily`, day granularity) but knows nothing about windows per account.

## Decisions

1. **Two sources, one blob.** Each account's latest usage state lives in the
   config-KV under `account_usage:<providerID>` (JSON `AccountUsageBlob`):
   the live scrape result (`source: "ollama"`) or the local window count
   (`source: "local"`). No new migration — the config table is the typed-free
   top layer; the typed shape is the Go struct.
2. **The scrape credential is a separate token-store kind** (`usage_cookie`,
   AES-256-GCM envelope like every token). It is NOT the API key: rotating
   one never touches the other. The SPA receives only a `has_usage_cookie`
   presence boolean; the cookie itself never leaves the store and the
   redactor masks `__Secure-session=…` / `"usage_cookie": "…"` shapes.
3. **Fixed scrape target, no SSRF surface.** The prober fetches
   `https://ollama.com/settings` only — the origin and path are package
   constants; no operator- or request-supplied URL reaches the fetch path.
   Outbound rides the httpguard client (redirect-guarded), 15 s timeout,
   4 MiB body bound.
4. **Refresh is on demand** (the Quota page's "Refresh now"). A background
   refresher may come later behind a server option — default off keeps the
   fixturesrv deterministic (no live upstream calls, no wall-clock state;
   e2e seeds bloom into the KV directly).
5. **Local fallback windows.** Accounts without a cookie (and custom types)
   show the UTC-month aggregate of their `usage_daily` rows (requests,
   failures, tokens) — honest local counting, clearly labeled as local.

## Consequences

- The executor's quota-exhaustion cooldown (HealthTracker) and the scraped
  reset windows converge on the same page: the health badge names the
  active cooldown reason, the usage bars show the scraped percentages.
- The webui AdminParams gained `HealthSnapshot` + `Prober` injection points;
  tests stub them (the adminEnv pattern).
- The Quota page renders % bars only when a blob exists; accounts never
  scraped render the local window with a "local" badge instead of guessing.
## Amendment (2026-10-17): the local fallback is retired

Decision 1's `source` value and decision 5 (local fallback windows) are
superseded: **the Quota card carries the account's own limits and nothing the
proxy measured itself.** The operator's reading — "nur das Limit von dem
Account" — is also the honest one: a locally counted request total answers a
question the account's own page never asked, and next to a scraped window it
reads as if it were part of the same limit.

What changed:

- `AccountUsageBlob` carries the scrape result only (`FetchedAt`, `Error`, the
  two windows). The never-written `Plan` and the never-written
  `MonthlyPct/MonthlyReset` go with it, as does the `"local"` source value no
  code path ever persisted.
- The account-usage DTO loses `source`, `local` and `plan`; the "live" badge
  is derived from the blob itself — `AccountUsageBlob.HasWindows()`. That
  derivation also fixes a defect the two-source design hid: an error-only
  blob (a failed scrape) used to be wrapped into `"live": {}` unconditionally,
  so the card drew two empty bars under a false "local" badge and the "Last
  scrape failed" empty state was unreachable. It is reachable now.
- `usage_daily`, `QueryUsageDailyByProvider` and `GET /overview` stay: local
  counting still exists, it just lives on the Overview page, where it belongs.
- The card's reset line becomes a countdown ("resets in 3h 12m") whose
  hover/focus tooltip names the exact instant **in the reader's browser
  timezone** — a presentation choice, not a data source: the timestamp was
  always the account's own.
- Superseded consequence: an account that was never scraped no longer renders
  a "local" window; it renders the empty state that says how to set the usage
  cookie.

## Amendment (2026-10-25): uniform windows, balance API and cooldowns

The blob grows a `schema_version` and a provider-neutral `windows[]` list
(`key`, `label`, `kind`, `remaining_pct`, `used_pct`, `remaining`, `limit`,
`unit`, `unlimited`, `reset_at`, `window_seconds`, `source`, `note`). The
legacy session/weekly/credit fields stay readable and are dual-written for one
release, and the old capitalized `FetchedAt`/`Error` keys still decode, so a
rollback binary renders the same card.

- **Ollama** prefers the documented `GET /api/balance` with the account's API
  key (session/weekly remaining percentages and reset instants; monthly USD
  balance and period) and keeps the settings-page cookie scrape as the
  fallback. The balance call sends the key only to the fixed ollama.com origin.
- **Firecrawl** credits and **Codex** header/stream facts are mapped into the
  same window shape; Codex now reads window lengths, extra limit families,
  credit headers and the `codex.rate_limits` SSE event.
- **Cooldowns** are observable: `HealthTracker.State` exposes the breaker and
  quota deadlines, and the usage DTO carries `health_until` plus a
  `cooldown` object. The state remains process-local by design — a restart
  clears the badge, while the provider's own reset instant stays on the window.
- **Refresh** is throttled per account and globally; a fresh cached reading
  answers `skipped: true` with a retry hint instead of calling upstream.
- **The Quota UI** groups by provider type, renders the remaining share as a
  shrinking bar (green → orange → red), shows the last-refresh age and the
  cooldown deadline, and keeps the local-usage prohibition from the previous
  amendment.
