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

- **OAuth flow endpoints** (`/api/admin/providers/{id}/oauth/start|poll|callback`)
  and live provider construction from stored config (incl. type-string validation)
  are Task 5 scope (plan §5.2), tracked as known gaps — the Providers UI renders
  the flow states already.
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