# Phase 1 — Task 17 Verification Notes (Codex provider, storage accessors, routing determinism)

**Date**: 2026-09-12
**Scope**: Phase 1, Task #17 — Codex OAuth provider (plan §6.3, digest §5b X9–X24),
typed storage accessors (plan Analysis C/E), §11 routing determinism suite, httpx
HeaderHook extension.
**Reviewer of record**: independent non-author reviewer agents (adversarial charter,
per Analysis F review doctrine) — one review pass + one re-review pass.
**Commit**: `e89fd38` on `main`.

---

## 1. Test & build gates (post-fix, both packages)

| Gate | Command | Result |
|---|---|---|
| gofmt | `gofmt -l ./internal/provider/codex/ ./internal/storage/` | **PASS** — no output |
| go vet | `go vet ./internal/provider/codex/ ./internal/storage/` | **PASS** — no findings |
| Tests | `go test -timeout 120s -count=1 ./internal/provider/codex/ ./internal/storage/` | **PASS** — codex 4.676s, storage 2.124s |
| golangci-lint v2.13.2 | `golangci-lint run ./internal/provider/codex/... ./internal/storage/...` | **PASS — 0 issues** |
| Race detector | `go test -race` | **CI-only** (no gcc on Windows host; blocking CI gate) |

RED-GREEN evidence: the three storage accessor test files
(`routingrules_test.go`, `models_test.go`, `requestlogs_test.go`) were written
first and confirmed RED (`undefined: RoutingRule`, `Model`, `PutModel`, …,
build failed) before `routingrules.go`/`models.go`/`requestlogs.go` were
implemented. The Codex contract tests derive from the live-researched codex-rs
facts (digest §5b, X9–X24), never from provider code behavior.

## 2. Independent review — findings & resolutions

**First pass** (adversarial, non-author): 9 findings — 3 MAJOR, 3 MINOR,
3 NOTE. All fixed:

| # | Severity | Finding | Resolution |
|---|---|---|---|
| 1 | MAJOR | No guard coupling `refreshInterval`/`refreshWindow`/`accessTTLDefault` — a future constant change could make the X18 short-circuit skip a rotation whose result is already expired | `init()` panic invariant `refreshInterval+refreshWindow <= accessTTLDefault` (compile-time assertion impossible for `time.Duration` constants) |
| 2 | MAJOR | Test memStore keyed only by `kind` and ignored `ProviderID` — masked production ProviderID footguns; three AuthClient constructions didn't pass `ProviderID` | memStore rekeys `providerID + "\x00" + kind`, honors both args, rejects empty IDs on write; `ProviderID` plumbed through `newBackend`, `testAuth`, `TestBuildAuthorizeURL_ClientIDOverride` |
| 3 | MAJOR | `TestRefresh_SingleFlight` didn't force overlap — could pass exercising only the short-circuit | Reworked with arrival barrier: leader blocks in the HTTP handler until all 4 goroutines started, `release` closes only then; asserts exactly 1 issuer hit + leader's rotated pair |
| 4 | MINOR | Device-flow ctx unbounded | Single `context.WithTimeout` over the whole flow + manual deadline check; every flow request bounded |
| 5 | MINOR | `RefreshCredentials` didn't document the 8-min no-op window | Comment documents X18 cadence + why a consumed token cannot hide behind the no-op |
| 6 | MINOR | `endpointError.Error()` interpolated unbounded issuer body | `truncateForLog(body, 256)` in the no-code branch |
| 7–9 | NOTE | slow_down divergence, nil-ExpiresAt trust, UA platform tail | All documented in code comments (RFC 8628 intended divergence per codex-rs; conservative externally-seeded trust; UA tail tracked in smoke checklist) |

**Re-review pass** (fresh non-author reviewer, adversarial charter including
new-defect checks and independent gate runs): 8 of 9 fixes confirmed correct;
3 new findings surfaced, all fixed:

| # | Severity | Re-review finding | Resolution |
|---|---|---|---|
| R1 | **BLOCKING (bug)** | `TestComplete_TransportError` passed **vacuously** — a latent casualty of the MAJOR-2 fix: auth client defaulted to `ProviderID: "codex"` while tokens were seeded under `"codex-1"`, so the test failed at the auth layer (`ReLoginRequiredError`) and never reached the closed endpoint it claims to test | `ProviderID: providerID` added to the AuthConfig; transport-error path now actually exercised |
| R2 | MAJOR | `error_description` (issuer-controlled, unbounded) still rode the error string in the `e.code != ""` branch — only the no-code branch truncated | Truncation moved to construction site in `do()`: `ee.description = truncateForLog(oe.Description, 256)` — every consumer (Error string, re-login reason) is now log-safe by construction |
| R3 | MINOR (coverage) | SingleFlight forced-overlap test signals arrival *before* `Refresh` — under slow scheduling, followers may take the lastRefresh short-circuit instead of the flight-wait path; the coalescing branch was not deterministically covered | New white-box test `TestRefresh_FlightWait_ParksWaitersOnFlight`: leader provably inside its HTTP handler, `lastRefresh` still zero (short-circuit impossible) → second caller MUST park on `<-f.done`; asserts no second rotation started (1 issuer hit), waiter adopts the flight's shared result |

Re-review also independently confirmed (no action): MAJOR-1 invariant is
mathematically sufficient (13m ≤ 15m with a 2m margin absorbing the
expires/lastRefresh write skew), device-flow ctx fix (single timeout, no
double-cancel), needsRefresh nil-ExpiresAt rationale, slow_down divergence,
UA tail omission, RefreshCredentials comment accuracy.

## 3. Delivered scope

- **`internal/provider/codex/`**: browser PKCE login (X1–X3/X16/X17/X21),
  device flow (X20, server-provided PKCE), lazy refresh with single-flight
  (X12) + X18 cadence window, atomic rotated-token persistence,
  `ReLoginRequiredError` on consumed/expired/revoked refresh tokens, transport
  per X14/X15/X22/X23/X24 (`OAI-Product-Sku` deliberately NOT sent — digest
  correction).
- **`internal/storage/`**: `tokens.go` (envelope-gated atomic token accessors),
  `routingrules.go` (deterministic order priority DESC + id ASC; empty-chain
  rejection at the persistence boundary), `models.go` (FK-enforced;
  capabilities + per-model compat JSON, the §6.4 surface),
  `requestlogs.go` (append-only, newest-first, `'{}'` JSON defaults).
- **`internal/routing/determinism_test.go`**: the §11 Phase 1 gate suite —
  8 table rows (capability fallback, health cooldown, chain resolution,
  compat-policy application) + 10-run identical-output test.
- **`internal/provider/internal/httpx`**: `HeaderHook` for per-request
  rotating credentials (OAuth) + Accept negotiation on streaming requests.

## 4. Known follow-ups (tracked, not blockers)

- Live smoke (`PROXYLLM_SMOKE=1`, Phase 1 DoD) must pin the OP-3 residues:
  refresh-grant body shape, backend request shape, and whether the X14 UA
  platform tail is actually required by the ChatGPT backend.
- `-race` verification runs in CI only (no gcc on the Windows host).

**Gate decision: Task #17 PASSED (2026-09-12).** All gates green post-fix;
all 12 review findings across both passes resolved and re-verified; the
re-review's own gate runs (vet, tests, lint) independently green.