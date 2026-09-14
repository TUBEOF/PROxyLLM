# Phase 2 Verification Report

**Phase**: 2 — WebUI
**Date**: 2026-09-13
**Gate**: per plan §11 Phase 2 DoD ("every §3.7 function usable end-to-end against a
live proxy; visual review against the dashboard kit previews, documented with
screenshots; independent review pass of the whole WebUI completed with blocking
findings resolved")
**Reviewer of record**: independent adversarial review pass with the Analysis F
charter (correctness vs. spec, race/backpressure on streaming paths, security vs.
Analysis G, test-quality audit, API ergonomics, docs consistency), performed as a
separate review pass over the Phase 2 deliverables by a reviewer that did not author
the code.

---

## 1. Deliverables (commits)

| Commit | Scope |
|---|---|
| `34afced` | Phase 2 best-practice research pass (`docs/research/phase-2-best-practices.md`) — session auth S-1..S-11, CSRF C-1..C-6, SPA serving P/E facts, visual regression V-1..V-7, all cited with source URL + access date |
| `9c12ff7` | Storage v2: provider/model/routing-rule CRUD, config KV, admins, sessions, client keys, usage_daily, log pagination |
| `1f05228` | WebUI session auth + CSRF middleware (research §10 S-/C- facts, RED-GREEN) |
| `cfbfeea` | Admin API over the storage v2 layer: full §5.2 resource surface, live routing reload hook |
| `560541a` | React SPA for all §9 routes (Overview, Providers, Models & Compatibility, Routing, Logs, Keys, Settings, Login) |
| `20abf2a` | SPA serving: history-routing fallback, strict CSP + security headers (research §5/P-1..P-5) |
| `6afe2a6` | WebUI Makefile targets + package scoping; ADR-002 implementation notes |
| `6aaa5fe` | ESLint config hardening (flat-config-independent) |
| `798873c` | Playwright visual regression suite + fixture server (routes × two viewports, design-rules assertions, empty states; baselines CI-only per V-3/V-5) |
| `f9d6429` | **Review fixes**: CSRF token re-issue on page refresh, readyForCapture selector, seed type strings, login-error token conformance |

## 2. Test & build gates

| Gate | Command | Result |
|---|---|---|
| Unit/integration tests | `go test -count=1 ./cmd/... ./internal/... ./webui` | **PASS** — all packages with tests green, incl. `internal/auth` 2.4–11.6 s (now 15 tests incl. 3 new CSRF re-issue tests), `internal/server/webui` 1.8–7.6 s, `internal/storage` ~16 s, `internal/server/httpapi`, `internal/e2e` 12/12 (Phase 1 acceptance unaffected) |
| Race detector | `go test -race` | **CI-only** (blocking in `.github/workflows/ci.yml`, ubuntu runner). Locally unavailable on the Windows host (CGO/gcc) — unchanged from Phase 1. |
| gofmt / go vet | `gofmt -l` / `go vet` | **PASS** — no findings over `./cmd/... ./internal/... ./webui` + fixturesrv |
| TS typecheck | `tsc --noEmit` (src + e2e) | **PASS** |
| ESLint | `eslint src` + e2e specs | **PASS** — 0 issues |
| Bundle budget | CI node script vs `dist/` | **PASS** — ~64 KB gzip initial vs 200 KB limit (CI-enforced, plan §9) |
| Visual regression | `npx playwright test -c playwright.config.ts` (+ empty config) | **CI-only by design** (V-3): baselines are generated exclusively in CI (`update-visual-baselines` workflow, `--update-snapshots`, bot commit) — no local baseline has ever been committed |
| golangci-lint / govulncheck / gitleaks / docker build | CI workflow steps | Blocking in CI; not run locally on the Windows host |

### Visual regression suite shape (commit `798873c` + `f9d6429`)

- **13 specs across 3 files**: `routes.spec.ts` (8: login + all seven authenticated
  routes), `design-rules.spec.ts` (4: dark-only, tokens-only on all routes, no emoji,
  see below), `empty/empty.spec.ts` (1: honest empty states, auth route).
- **Two projects** (desktop-chrome 1280×720, iphone-13 390×844 DPR 3) → 26 screenshot
  assertions per config, plus the empty-state config.
- **Design-rule assertions, not just pixels**: computed-style token scan (color /
  backgroundColor / borderTopColor against the colors_and_type.css palette),
  color-scheme dark-only, no-emoji scan, plus time freeze to a pinned instant
  (`FROZEN_DATE = 2026-09-13T12:00:00Z`) and value masks on all volatile elements
  (`.kpi-value`, `.bar-val`, `.spark`, `.ring`, log timestamps).
- **Real assembly, no network mocking**: the fixture server is a real PROxyLLM
  instance (embedded SPA, real admin API, real session/CSRF auth, seeded SQLite) —
  the browser drives exactly the code path an operator's browser drives.
- **readyForCapture** (fixed in `f9d6429`): the gate now genuinely waits for loading
  states to clear (`.loading` lazy boundary + `.empty:has-text("Loading")`) — the
  pre-fix selector asserted a class that never existed in the SPA, so the screenshot
  gate never actually waited.

### Empty states are honestly empty

Honest-empty-state doctrine (plan §9): the empty suite runs against a seed=false
store (admin account only). Overview shows zeroed KPIs with neutral comparison and
no fabricated metrics; every table route renders the kit's empty-state pattern with
an honest explanation, never fake data.

## 3. Independent review pass — findings & resolutions

The adversarial pass (charter per Analysis F) covered the full Phase 2 surface:
auth/session middleware, admin API, SPA serving, the React app (all routes, hooks,
api client, overlays, icons, types), the visual suite + fixture server, CI wiring,
CSS layers and `index.html`, and the test files as a test-quality audit (do tests
derive from spec, or mirror code?).

| # | Finding | Severity | Resolution |
|---|---|---|---|
| 1 | CSRF token lost on page refresh: the SPA held the token only in memory while the server issued it solely in the login/bootstrap body — after an F5 every mutation (logout included) failed with a missing-token error at a live session. The MAC needs the master key, so the SPA cannot reconstruct the token from the cookie's random half. | **BLOCKING** | Fixed in `f9d6429` (RED-GREEN): `RequireAdmin` re-issues the full session-bound token as an `X-CSRF-Token` header on every authenticated response (`csrfRandomFor` reuses the csrf cookie's random for token stability, mints a replacement cookie otherwise); `api.ts` adopts it via `adoptReissuedToken`. No header on 401 responses. 3 new tests: re-issue for live session + mutation round-trip, session binding of the re-issued token, no re-issue unauthenticated. |
| 2 | `readyForCapture` asserted `.empty-state:has-text("Loading")` — a class that does not exist in the SPA. The visual gate never actually waited for loading states to clear; screenshots could capture mid-load states. | **BLOCKING** (visual gate) | Fixed in `f9d6429`: waits on `.loading` (lazy boundary) and `.empty:has-text("Loading")` — both classes verified against the SPA's actual rendering (EmptyState component, Suspense boundary). |
| 3 | Seed provider type strings used underscore forms (`custom_openai`) while the WebUI editor's canonical vocabulary (`PROVIDER_TYPES` in Providers.tsx) is hyphenated (`custom-openai`, `ollama-cloud`, `codex-oauth`) — a UI save over a seeded provider would silently rewrite the type. `putProvider` does not validate type strings (Task 5 scope). | MINOR | Fixed in `f9d6429`: seed uses the canonical hyphenated forms. Go storage-level tests keep their `custom`/`ollama`/`codex` strings (storage-layer concern, deliberately untouched). |
| 4 | `.login-error` used a raw `oklch(15% 0.03 25 / 60%)` background despite the tokens-only rule the same file's header declares. | MINOR (design rules) | Fixed in `f9d6429`: derived from the `--status-down` token via `color-mix`. Token scan over all TSX/TS sources: clean; `kit.css` stays verbatim per K-1 (its one `#fff` is the kit's own). |
| 5 | (from the pre-`798873c` review round, resolved there) testDir collision between the two Playwright configs; seed drift against the frozen clock; no-op snapshot replace masking real diffs. | BLOCKING/MINOR | Resolved in `798873c` (separate `.artifacts`/`.artifacts-empty` dirs, seed anchored to `FROZEN_DATE`, replace-with-diff semantics). |

Test-quality audit result: admin API tests derive from the §5.2 contract and the
S-/C- facts (bootstrap-first-admin-only, uniform 401 shape, display-once keys,
FK guarantees as 400s, reload-only-after-successful-write, inclusive usage bounds,
newest-first pagination with filters, CSRF/415 through the real mux) — not from
implementation behavior. SPA tests run against a synthetic dist tree with no
dependency on a Node build. No findings from the audit.

## 4. What is deliberately not in this gate

- ~~**OAuth flow endpoints** (`/api/admin/providers/{id}/oauth/start|poll|callback`)
  and live provider construction from stored config (incl. type-string validation)
  are Task 5 scope (plan §5.2), tracked as known gaps — the Providers UI renders
  the flow states already.~~ **Gap #1 closed** post-gate (2026-09-14) — see §6.
- **usage_daily writer** in the request-log path (stats flow from real traffic) —
  Task 5 scope.
- **Config-file/DB layering in `internal/config`** (WebUI settings → effective
  config) — Task 5 scope.
- OP-3/OP-4 live pinning remains ENV-gated smoke scope (`PROXYLLM_SMOKE=1`), per
  plan §11 Phase 1.

## 5. Verdict

All blocking findings resolved and re-verified (full Go suite, tsc, ESLint green
after the fix commit); the remaining minor findings are resolved in the same commit.
The Phase 2 gate is met for the shipped scope: every §3.7 function is usable
end-to-end against the assembled proxy via the real admin API, the visual regression
suite is CI-wired with design-rule assertions, and the independent review pass is
complete with blocking findings fixed. Phase 2 close-out: **PASS**, with the Task 5
gaps above explicitly carried.

## 6. Task 5 Gap #1 close-out — OAuth flow endpoints (2026-09-14)

The §5.2 OAuth sign-in surface is implemented as a **stepwise device flow**
(RFC 8628) instead of the planned browser-callback route — pinned decision:
RFC 8628 needs no inbound endpoint, so it is the Docker-compatible primary
path; the loopback browser flow (`codex.LoginFromBrowser`, X1–X5) stays a
local-run option, and the planned `oauth/callback` admin route is
**deliberately not implemented** (deviation from plan §5.2, documented here
instead).

### Deliverables (commits)

| Commit | Scope |
|---|---|
| `44e63c9` | `codex`: stepwise device flow — `StartDeviceLogin` / `DeviceSession.Poll`, one issuer round trip per poll, throttled to the issuer interval (RFC 8628 §3.5), exchange+persist inside the poll flight, concurrent-poll coalescing (X20 wire facts) |
| `1e6aa7f` | Admin API `POST /providers/{id}/oauth/start` + `oauth/poll`, in-memory flow registry (replace semantics, drop-on-terminal), 10 contract tests |
| `efece10` | SPA: `CodexSignIn` in the Providers drawer — start → user_code + verification link → issuer-paced polling → terminal badges, restart via replace; tokens never reach the browser |
| `983b21e` | **Review fix** (blocking, found by this close-out's adversarial pass): bodyless admin mutations 415'd — see finding 6 |

### HTTP contract (pinned)

- `start` → `200 {user_code, verification_url, interval_seconds}` | `400`
  (non-codex type, missing issuer, invalid config JSON, device-login-disabled) |
  `404` | `502`.
- `poll` → `200 {"status":"pending"}` | `200 {"status":"approved","account_id"}`
  (tokens exchanged AND persisted before this returns) | `400` "no active oauth
  flow — start first" (never started / already approved — single-use code — /
  previously failed) | `502` (terminal issuer failure; flow dropped).
- Flow state is **in-memory only** (issuer-side pending authorization, never a
  secret; tokens ride the encrypted `provider_tokens` rows); `deleteProvider`
  removes an active flow; a second `start` replaces the active one.
- Uniform `{"error": ...}` bodies; `RequireAdmin` (session + CSRF) gates both.

### Adversarial review findings (close-out pass over `44e63c9`+`1e6aa7f`+`efece10`)

| # | Finding | Severity | Resolution |
|---|---|---|---|
| 6 | Bodyless mutations 415'd in the browser: `RequireAdmin` demanded `application/json` on EVERY mutation (C-4 as written), but fetch sends no Content-Type without a body — logout, provider delete, key revoke, `oauth/start`, `oauth/poll` all failed. The admin test helper set the header on bodyless requests with a comment claiming "the SPA helper sets it always" — it never did: the test mirrored a wishstate, not the real client (the exact defect the test-quality audit exists to catch). | **BLOCKING** | Fixed in `983b21e`: an ABSENT Content-Type is legal (no cross-site form vector can omit it — form posts always carry urlencoded/text/plain, which still 415; the CSRF check gates bodyless mutations independently). `authedCall` now mirrors fetch exactly. New tests: `TestMutation_ContentTypeRules` (bodyless-OK, text/plain-415, bodyless-no-CSRF-403) + a through-the-mux logout regression. |
| 7 | Race `deleteProvider` ↔ in-flight `Poll`: token persist after the provider row is gone hits the `provider_tokens REFERENCES providers` FK → poll error → 502 + flow drop. | MEDIUM | Accepted as designed: an honest, operator-caused failure of a flow the operator destroyed; no orphan rows, no secret exposure, uniform error. Documented here, no code change. |
| 8 | Race `oauthStart` replace ↔ in-flight `Poll` on the old session: a late issuer approval can still exchange+persist under the replaced flow (last-approval-wins). | MEDIUM | Accepted: operator-driven, authorization codes are single-use, the new start kills the old user_code issuer-side anyway. Documented. |
| 9 | `cfg.Issuer` from provider config is used unchecked in issuer URLs — same SSRF class as the custom-openai `base_url` guard. | MEDIUM (deferred) | Tracked into Task 5 Gap #2 (provider-type validation at `putProvider` + live registration), where the issuer/base-URL allowlist + private-address guard is implemented once for both types. |
| 10 | Client disconnect mid-poll pins `context.Canceled` as the session's terminal error. | LOW | Accepted: self-healing via "Start over" (replace semantics). Documented. |

Gates at close-out: gofmt/vet clean, full Go suite green
(`./cmd/... ./internal/...`), tsc + ESLint green for the SPA commits. The
providers.png visual baseline is unaffected — drawer states are not snapshotted
(the table screenshot is).
## 7. Task 5 Gap #2 close-out — live provider construction & type validation (2026-09-14)

Stored provider rows now become live routing targets without a restart, and every
provider write is validated against the canonical provider types before storage
(closing finding 9's issuer/base-URL SSRF gap together with the write-path gap).

### Deliverables (commits)

| Commit | Scope |
|---|---|
| `47c1f41` | `webui`: provider-type validation at `putProvider` — canonical types (`custom-openai` / `ollama-cloud` / `codex-oauth`), strict config parse (`DisallowUnknownFields`), required `base_url`/`issuer`, cross-type URL prohibitions, strict capability key names, private-address SSRF guard on both `base_url` and `issuer` with documented opt-out (Analysis G control 5). Top-level `api_key` on the PUT routes into the encrypted `provider_tokens` store — config never carries secrets. Plus the `ollamacoud` → `ollamacloud` doc-comment fix. |
| `064cc16` | `server`: `provider.LiveRegistry` (thread-safe runtime-mutable registry; statics survive reloads, dynamics atomically swapped), startup construction from stored rows (same validator as writes, keys from the token store, codex auth keyed to the provider's own ID, bad rows skip+log instead of startup death), reload hook after every successful provider write/delete (last known-good kept on failure). |

### Validation contract (pinned)

- Unknown type, missing `base_url` (custom-openai) / `issuer` (codex-oauth),
  unknown config keys, unknown capability names, cross-type URL fields → **400**
  with an operator-facing error naming the offending field.
- `base_url`/`issuer` must be absolute http(s) URLs resolving to a non-private
  address; loopback/RFC1918/link-local (incl. 169.254.169.254) → **400** with the
  opt-out hint. `PROXYLLM_ALLOW_PRIVATE_PROVIDER_URLS=1` disables the private check
  (not the scheme/host check) — the documented operator escape (Analysis G
  control 5).
- `ollama-cloud` with an empty config is a legal save (hosted default URL).
- The API-layer guard resolves DNS at write time; unit tests that are not about
  the guard run under the opt-out so they do not depend on DNS (the guard itself
  is pinned by `TestPutProvider_PrivateURLsRejected`).
- Storage's secret-shaped-config rejection (Analysis G control 2) stays in place
  as defense-in-depth (its own tests in `internal/storage`); the strict API parser
  now rejects unknown/nested keys before it is even reached.

### Live-registry contract (pinned)

- Startup: every stored row constructs (custom/ollamacloud/codex paths); a row
  that fails parse is skipped with an error log — never a startup death; keys come
  exclusively from `provider_tokens` kind `access_token` (missing row = keyless
  provider, legal).
- Write/delete → reload hook: the next request resolves against the NEW set;
  a failed reload keeps the last known-good set (error logged).
- Static `WithProvider` registrations are never removed by a reload.

### New tests

Server wiring (real mux, end-to-end): `TestServer_StoredProviderRoutesWithoutWithProvider`
(provider from store routes traffic with the encrypted-store key, no
`WithProvider`), `TestServer_ProviderWriteReloadsLiveRegistry` (pre-save 404 →
post-save live routing with the new key). Admin API: `TestPutProvider_*` (5
validation cases incl. SSRF + opt-out), `TestProviderWriteReloadsLiveRegistry`
(hook on write/delete, never on rejected write). Existing CRUD/model tests
updated to the canonical type strings.

Gates: gofmt/vet clean, full Go suite green (`./cmd/... ./internal/...`). No SPA
change (the SPA save path sends the same PUT shape; `api_key` is accepted
top-level from the next UI iteration).

### Adversarial review findings (Gap #2 pass over `47c1f41`+`064cc16`)

| # | Finding | Severity | Resolution |
|---|---|---|---|
| R1 | The reload/startup construction re-ran the WRITE-path SSRF guard, which resolves DNS: a transient DNS failure during a reload could skip previously-saved providers (skip+log) and empty the live registry → 404s on all their models. | **BLOCKING** | Fixed in `00d5592`: guard split — `ParseProviderConfigStatic` (construction path) keeps full shape checks but never resolves DNS; rows were already guarded at write time. |
| R2 | Provider PUT decoded the top-level body non-strictly: a typo'd `apiKey` returned 200 while silently dropping the credential. | MEDIUM | Fixed in `00d5592`: strict `DisallowUnknownFields` decode on the PUT body; `TestPutProvider_UnknownTopLevelFieldRejected` pins it. |
| R3 | Token-write failure after a successful provider PUT → 500 with the provider row persisted but the reload skipped (dormant until next write). | MEDIUM | Accepted: FK keeps the write ordered, the operator retries, no partial state. Documented. |
| R4 | `providerAPIKey` logged via the package-level slog default instead of the injected logger. | MINOR | Fixed in `00d5592`. |
| R5 | `ParseProviderConfig` accepted trailing JSON after the config object (Decode reads only the first value). | MINOR | Fixed in `00d5592` (EOF token check). |
| R6 | Write-time DNS guard is TOCTOU-prone (DNS rebinding between save and request). | MEDIUM (deferred) | Documented limitation for v1: the runtime dial path is a separate hardening layer (Phase 3 security pass). The write-time guard stops the direct save-a-private-endpoint vector. |

Gates after the fixes: gofmt/vet clean, full Go suite green
(`./cmd/... ./internal/...`). **Gap #2 close-out: PASS.**

---

## 8. Gap #3 close-out — usage_daily writer in the request-completion path

**Gap**: the storage layer shipped `IncrUsageDaily`/`QueryUsageDaily` (Analysis C
schema, migration v1), but no caller ever incremented the table — the Overview
dashboard's data source was write-orphaned. **Status: CLOSED (commits `29ec360`,
`1384bee`).**

### Deliverables

| Change | Where |
|---|---|
| usage_daily delta in `writeRequestLog` | `internal/server/httpapi/httpapi.go` |
| day = today (UTC), same tuple as the log row, requests +1, failures +1 at status >= 400 | `internal/server/httpapi/httpapi.go` |
| Token deltas only when usage exists; pre-upstream requests aggregate zeros on their tuple | `internal/server/httpapi/httpapi.go` |
| Streaming terminal usage feeds the same counters (O4) | via `terminalUsage`/`usageArg` plumbing (no call-site change) |

### Contract (spec-derived, Analysis C/E)

- Every observable request counts: requests +1 on every `writeRequestLog` call
  (success, route error, pre-route 400, count_tokens heuristic).
- Failures: status >= 400 → failures +1 (any client-visible error, Analysis E —
  not only upstream failures).
- Tuple accumulation: two requests on the same (day, key, provider, model) produce
  ONE row — PK semantics, never row-per-request drift.

### New tests (`internal/server/httpapi/usage_test.go`)

`TestUsageDaily_IncrementedOnChatSuccess` (golden 1250/77/8192),
`TestUsageDaily_IncrementedOnFailure` (failures=1), `TestUsageDaily_IncrementedOnStreamSuccess`
(exact 1250/128), `TestUsageDaily_IncrementedOnMessagesSuccess` (Anthropic surface
feeds the same aggregation), `TestUsageDaily_AccumulatesSameTuple` (2 requests → 1 row,
requests=2, tokens=2500). All RED first (rows = 0 before the fix), then GREEN.

### Adversarial review findings (Gap #3 pass over `29ec360`)

| # | Finding | Severity | Resolution |
|---|---|---|---|
| R1 | chat/messages Decode-error path (400 before the model is known) writes no log/usage row. | MINOR | Accepted: pre-existing behavior, consistent on both surfaces, model unknown at that point. |
| R2 | count_tokens counts as a request (requests+1). | — | Correct: it is a served request (Analysis E). |
| R3 | Streaming test asserted `!= 0` instead of exact golden numbers. | MINOR (test quality) | Fixed in `1384bee`: exact 1250/128 pinned. |

Gates: gofmt/vet clean, full Go suite green (`./cmd/... ./internal/...`).
**Gap #3 close-out: PASS.**
