# ADR-012: Per-account usage snapshot (Quota page)

Status: Accepted (2026-10-06)
Amends: ADR-010 (capability fallbacks), complements ADR-009 (accounts/strategies)

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