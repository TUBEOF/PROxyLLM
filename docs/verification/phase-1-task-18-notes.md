# Phase 1 — Task 18 Verification Notes (httpapi surfaces, server wiring, Firecrawl emulation)

**Date**: 2026-09-12
**Scope**: Phase 1, Task #18 — client-facing httpapi surfaces (plan §5.1:
OpenAI chat/completions + models + responses; Anthropic messages +
count_tokens + dual-shape models), server assembly (§4.1), per-model compat
policies in the request path (§6.4), and web_search/web_fetch emulation via
the Firecrawl fallback (§6.2).
**Reviewer of record**: independent non-author reviewer agents (adversarial
charter per Analysis F) — two passes over item (d); item (e) reviewed by the
wiring-test discipline below.
**Commits**: `a774248` (surfaces + wiring), `db789ba` (review fixes), `f2518ad`
(§6.2 emulation), this close-out (`n3`/`n5` coverage + notes).

---

## 1. Test & build gates (final state, whole repo)

| Gate | Command | Result |
|---|---|---|
| gofmt | `gofmt -l .` | **PASS** — no output |
| go vet | `go vet ./...` | **PASS** — no findings |
| Tests | `go test ./... -count=1` | **PASS** — 16 packages, 0 failures |
| golangci-lint v2.13.2 | `golangci-lint run` | **PASS — 0 issues** |
| Race detector | `go test -race` | **CI-only** (no gcc on Windows host; blocking CI gate) |

## 2. Independent review — findings & resolutions (item d, commits a774248/db789ba)

**First pass** (adversarial, non-author): 2 blockers, 3 majors, 5 minors,
6 notes. All blockers/majors and all actionable minors fixed in `db789ba`:

| # | Severity | Finding | Resolution |
|---|---|---|---|
| B1 | BLOCKER | `relayStream` ignored encoder write errors — a disconnected client kept the upstream pump relaying into a dead connection | write error stops the event pump |
| B2 | BLOCKER | terminal error event was not treated as terminal — a misbehaving provider keeping the channel open could push frames after the error | nothing relayed after the terminal error event; empty error types normalize to `upstream_error` |
| M1 | MAJOR | test harness used decoupled HealthTracker instances (router-gate ≠ executor-report) — would mask health-gated failover bugs | one shared HealthTracker gates the router and receives executor reports (harness + server wiring) |
| M2 | MAJOR | Firecrawl `decodeAPIError` could echo non-JSON upstream bodies into error strings | size + status only; raw bodies never echoed |
| M3 | MAJOR | count_tokens wrote no request-log row | every count_tokens request logs (decode failure and heuristic answer) |
| m1/m2 | MINOR | dead `itoa`/`jsonInt` helpers + unused imports in messages.go; `redactedMessageCompat` wrapper | removed/collapsed |
| m3 | MINOR | readBody relied on `http.MaxBytesReader` reachability; 32 MB cap now structural | `LimitReader` with explicit 413 on overrun |
| m4 | MINOR | writeJSON hardcoded the OpenAI error shape on marshal failure | writeJSON takes the surface error writer |
| m5 | MINOR | `isAnthropicSurface` matched by prefix — `/v1/messages-anything` would switch shapes | exact match on `/v1/messages` and `/v1/messages/...` |

**Notes disposition**:

- n1 (no timeout on caller-supplied custom HTTP clients for the Firecrawl
  client): addressed by the default-client timeout; caller-supplied clients
  remain caller responsibility — documented.
- n2 (models.meta.json dated 2026-09-12 vs. fixture corpus 2026-09-10):
  **intentional** — that fixture was captured live on 2026-09-12; the
  meta file records the true access date.
- n3 (no `TestResponses_StreamMidStreamError`): **closed in this close-out**
  — test added, GREEN. Derives from O7 (flat `{type,code,message}` error
  event) + Analysis E no-swap discipline: 200 + `event: error`, error
  terminal (nothing relayed after), flat shape asserted.
- n4 (maskKeyPrefixes blunt but safe): accepted, no action.
- n5 (no auth-disabled path test through httpapi): **closed in this
  close-out** — `TestMessages_AuthDisabledNoKeyPath` added, GREEN. Keyless
  requests pass on both surfaces with the opt-out applied; the auth-on
  default still 401s keyless requests (control).
- n6 (Trace copies): verified correct — no finding.

## 3. Item (e) — Firecrawl emulation (§6.2), verification summary

- **Config surface**: `PROXYLLM_FIRECRAWL_API_KEY` (secret — ENV-only, never
  in the exported Config struct) + `PROXYLLM_FIRECRAWL_BASE_URL`
  (`cfg.Firecrawl.BaseURL`). Config tests cover both.
- **Emulator adapter** (`internal/tools/firecrawl/emulator.go`): v2
  Search/Scrape behind the routing-declared `Emulator` interface
  (dependency inversion — the adapter does not import routing; compile-time
  interface proof lives in the adapter tests). 8 adapter tests: domain-filter
  mapping, markdown scrape normalization (text/plain, retrieval timestamp),
  citations flag, upstream errors (429 + `success:false` on 2xx), golden-shape
  decode, empty-search not an error.
- **Emulation loop** (routing): executor round trip — emulated tool call →
  emulator → protocol-conform result blocks appended → re-dispatch; budget
  `maxEmulationRounds` 32; guards: per-tool max_uses, input validation,
  prior-context URL guard for web_fetch, domain allow/block lists; every
  error answers with the documented Anthropic tool error codes — never
  silent; every substitution lands in route_trace.
- **Gating chain verified end-to-end**: per-model compat policy
  `server_tool_emulation:"emulate-firecrawl"` in the stored models row →
  httpapi `compatFor` per request → `applyEmulation` marks the defs
  `Emulated` → executor loop engages. The server wiring test
  (`TestServer_WithEmulatorAnswersWebSearch`) proves the full round trip
  through the Anthropic surface: synthesized `server_tool_use` +
  `web_search_tool_result` blocks, Firecrawl hit, text answer.
- **Assembly discipline**: `WithEmulator` wired only when a Firecrawl key is
  configured; without one, emulated tools degrade to the documented
  "unavailable" error results and startup logs a loud Warn — never silently
  off.
- **EncryptedContent discipline**: Firecrawl returns titles/URLs, not
  provider-encrypted page content — the emulator never fabricates opaque
  provider data.

## 4. Known follow-ups (tracked, not blockers)

- E2E acceptance per Phase 1 DoD (Task #19): official OpenAI SDK + Claude
  Code against the proxy (mock in CI, live smoke ENV-gated
  `PROXYLLM_SMOKE=1`), incl. the Claude Code → non-Claude path with
  route_trace showing thinking handling.
- `-race` verification runs in CI only (no gcc on the Windows host).
- server.New loads routing rules once at assembly; a WebUI rule-edit refresh
  path lands with Phase 2 (admin API owns rule mutation then).
- `runChat`'s raw []byte parameter is still unused (chat.go) — cleanup with
  the Phase 2 admin-API work.

**Gate decision: Task #18 PASSED (2026-09-12).** All gates green; all 11
first-pass findings resolved; deferred notes n3/n5 closed with GREEN tests in
this close-out; item (e) verified end-to-end.