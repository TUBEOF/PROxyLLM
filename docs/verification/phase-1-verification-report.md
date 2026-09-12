# Phase 1 Verification Report

**Phase**: 1 — Proxy core
**Date**: 2026-09-12
**Gate**: per plan §11 Phase 1 DoD ("official OpenAI SDK client AND Claude Code run
end-to-end against the proxy (mock in CI; live smoke ENV-gated) — including a Claude
Code → non-Claude model path with route_trace showing thinking handling; independent
review pass completed, blocking findings resolved, verification report archived")
**Reviewer of record**: independent non-author reviewer agent (adversarial charter,
per Analysis F review doctrine) — separate reviewer agents reviewed Tasks 17, 18,
and the E2E/smoke acceptance layer (Task 19) reported here.

---

## 1. Test & build gates

| Gate | Command | Result |
|---|---|---|
| Unit/integration tests | `go test ./... -count=1` | **PASS** — all packages with tests green: `internal/auth` 1.540s, `internal/config` 0.564s, `internal/e2e` 2.166s (12/12), `internal/observability` 0.823s, `internal/provider/codex` 4.666s, `internal/provider/custom` 1.808s, `internal/provider/ollamacoud` 1.745s, `internal/routing` 0.735s, `internal/server` 1.049s, `internal/server/httpapi` 4.431s, `internal/smoke` 0.255s (3 SKIP by design), `internal/storage` 2.493s, `internal/testutil/mockprovider` 1.815s, `internal/tools/firecrawl` 1.891s, `internal/translate/anthropic` 2.289s, `internal/translate/ollama` 1.489s, `internal/translate/openai` 1.514s, `internal/translate/sse` 0.871s |
| Race detector | `go test -race ./...` | **CI-only** (GitHub Actions ubuntu runner; blocking in `.github/workflows/ci.yml`). Locally unavailable: `-race` requires CGO + gcc, no toolchain on the Windows host. |
| gofmt | `gofmt -l .` | **PASS** — no output |
| go vet | `go vet ./...` | **PASS** — no findings |
| golangci-lint v2.13.2 | `golangci-lint run` | **PASS — 0 issues** |
| govulncheck / gitleaks / docker build + healthcheck | CI workflow steps | Blocking in CI (`.github/workflows/ci.yml`); not run locally on the Windows host |

## 2. E2E acceptance evidence (Phase 1 DoD)

Official-SDK acceptance suite (`internal/e2e`, commit `f32e226`): the **official**
client SDKs — `openai-go/v3 v3.61.0` and `anthropic-sdk-go v1.72.0` (direct deps in
`go.mod`, pinned by `go mod tidy`) — drive a fully assembled proxy (auth middleware,
httpapi surfaces, router, executor, HealthTracker) over real HTTP against the
scripted mock upstream. **12/12 GREEN**, zero hand-rolled HTTP on the client side:

OpenAI SDK (`internal/e2e/openai_sdk_test.go`):

| # | Test | Acceptance pinned |
|---|---|---|
| 1 | `TestOpenAISDK_ChatRoundTrip` | chat completions relay (id + text + usage 12/7) |
| 2 | `TestOpenAISDK_ChatStreaming` | streamed deltas relayed, SDK assembles text |
| 3 | `TestOpenAISDK_ResponsesRoundTrip` | Responses wire → IR → chat-wire upstream → IR → Responses (bidirectional translation, usage mapped) |
| 4 | `TestOpenAISDK_ResponsesStreaming` | semantic event sequence `response.created` → `output_text.delta` → `response.completed` with terminal usage 1250/128 (golden contract) |
| 5 | `TestOpenAISDK_ModelsList` | dual-shape `/v1/models` resolves stored rows |
| 6 | `TestOpenAISDK_NoRouteTypedError` | no-route → typed SDK `APIError`, 4xx (never 5xx) |

Anthropic SDK (`internal/e2e/anthropic_sdk_test.go`) — the SDK that backs Claude
Code's `ANTHROPIC_BASE_URL` gateway path, against an OpenAI-chat-wire upstream
(the cross-surface OP-2 translation):

| # | Test | Acceptance pinned |
|---|---|---|
| 7 | `TestAnthropicSDK_MessagesRoundTrip` | Messages.New round trip, text block out, `stop_reason: end_turn`, usage translated |
| 8 | `TestAnthropicSDK_MessagesStreaming` | ordered `message_start` → `content_block_delta(text_delta)` → `message_delta` → `message_stop` |
| 9 | `TestAnthropicSDK_CountTokens` | count_tokens answers positively (heuristic, marked) on non-Anthropic upstream |
| 10 | `TestAnthropicSDK_ThinkingStripOnNonClaude` | **THE DoD acceptance**: thinking request (budget 1024) on `thinking_handling:"strip"` model → succeeds, upstream body contains **no thinking artifacts**, `route_trace` carries `compat_policy {from: thinking_handling, to: strip}` (§6.4: never silent) |
| 11 | `TestAnthropicSDK_ThinkingBlockOnNonClaude` | `thinking_handling:"block"` → protocol-conform 4xx, **0 upstream requests** |
| 12 | `TestAnthropicSDK_NoRouteTypedError` | no-route → typed SDK error, 4xx |

**Harness note (environment-driven defect found and fixed during Task 19)**: the
initial run failed 5 Anthropic tests with 401. Root cause fully verified against
the pinned SDK source: this machine runs inside a Claude Code environment, so
`ANTHROPIC_AUTH_TOKEN` is set; the SDK's `DefaultClientOptions` applies the env
token as a default `Authorization: Bearer` header *before* caller options, and that
header survives `option.WithAPIKey` (which only adds `X-Api-Key`); the proxy's
documented header precedence (Bearer wins, §5.1 C5) then validated the *ambient*
token instead of the test key. Fix: `t.Setenv` empty-string neutralization of
`ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN`/`ANTHROPIC_BASE_URL` in the harness
(empty string suppresses the SDK's env default because of its `LookupEnv` +
`v != ""` guard). This is a test-environment neutralization, not a proxy defect —
the proxy behaved per spec.

## 3. Live smoke skeleton (OP-3 / OP-4 pinning)

`internal/smoke/smoke_test.go` (commit `f32e226`): ENV-gated live smokes pin the two
research gaps §2.9 leaves open. They **SKIP by default and never run in default CI**
(verified: `go test ./internal/smoke/` → 3 SKIP; CI's `go test -race ./...` runs the
same skip path because `PROXYLLM_SMOKE` is unset).

| Smoke | Pins | Production path exercised | Credentials |
|---|---|---|---|
| `TestSmoke_CodexRefreshGrant` (OP-3) | refresh-grant body at `{issuer}/oauth/token` | production `codex.NewAuthClient` + `Refresh` + `AccessToken` over the encrypted store (test-only envelope in tempdir) | `PROXYLLM_SMOKE_CODEX_REFRESH_TOKEN` (never logged); issuer/backend defaults overridable |
| `TestSmoke_CodexBackendShape` (OP-3) | ChatGPT backend request shape (`/codex/responses`, mirror headers) | production `codex.New` + `Complete` | same as above |
| `TestSmoke_OllamaNativeStream` (OP-4) | native `/api/chat` NDJSON wire: terminal `done:true` + `done_reason` + usage | production `ollama.Codec{}.EncodeRequest` + `ollama.NewStreamDecoder` | optional `PROXYLLM_SMOKE_OLLAMA_URL`/`_KEY`/`_MODEL`; local default needs no key |

Smoke discipline per §5.3/§8: credentials exclusively from ENV, never asserted-on
opaque token values, no raw upstream bodies echoed (status codes and decoded field
names only), 30 s client leashes, failures direct escalation per plan §2.9 rather
than test adaptation.

**Status: pinned-by-smoke, pending live run.** The live executions require
`PROXYLLM_SMOKE=1` plus a real Codex refresh token and/or a live Ollama instance —
operator-gated, not part of this local verification. The suite's skip-by-default
behavior is verified locally and the CI workflow (`.github/workflows/ci.yml`) never
sets `PROXYLLM_SMOKE`.

## 4. Independent code review (Analysis F doctrine)

Tasks 17 and 18 review outcomes are in docs/verification/phase-1-task-17-notes.md /
phase-1-task-18-notes.md; their resolutions are commits db789ba (B1/B2, M1–M3,
m1–m5) and 3d4a9ef.

**Task 19 (E2E/smoke acceptance layer)** — adversarial review by an independent
non-author reviewer agent, charter per Analysis F (spec-derivation audit,
test-quality audit, harness-correctness). **Verdict: PASS — no blocking
findings.** The reviewer explicitly confirmed clean: ENV-gating of smokes
(skip-by-default, never in CI), credential handling (ENV-only, never logged,
never asserted-on), usage of production paths (codex.NewAuthClient /
ollama.Codec / encrypted store), and the ambient-credential harness fix
(ANTHROPIC_* neutralization — "correct and complete").

Two MAJOR + eight MINOR test-quality findings (none gate-blocking), all
resolved in commit `2d5f1c8`:

| # | Severity | Location | Finding | Resolution |
|---|---|---|---|---|
| 1 | MAJOR | e2e/harness_test.go | `chatResponseBody()` hand-rolled body contradicted the golden fixtures, violating the harness's own "never from implementation behavior" claim | body now serves `testdata/golden/openai/chat-response.json` verbatim; all round-trip assertions updated to canonical values (chatcmpl-abc123, text, tool_calls, usage 1250/77) incl. the Anthropic `tool_use` block + `stop_reason:"tool_use"` translation |
| 2 | MAJOR | e2e/anthropic_sdk_test.go | streaming test asserted presence booleans only, not the ordered A5–A7 sequence | phase counter: message_start → content_block_delta → message_delta → message_stop must appear in order |
| 3 | MAJOR | e2e/openai_sdk_test.go | same ordering gap for O7 | phase counter: response.created → output_text.delta → function_call_arguments.delta → response.completed |
| 4 | MINOR | e2e/openai_sdk_test.go | terminal usage asserted InputTokens only | both numbers pinned: 1250/128 |
| 5 | MINOR | e2e/harness_test.go | dead `env.key` / `env.t` fields (grep-confirmed unread) | removed |
| 6 | MINOR | e2e/harness_test.go | missing rationale why OPENAI_API_KEY/OPENAI_BASE_URL are not neutralized | comment added: openai-go last-wins field semantics (verified against pinned module source), unlike anthropic's separate-header behavior |
| 7 | MINOR | e2e/anthropic_sdk_test.go | CountTokens asserted positivity only | pins `"estimation":"heuristic"` via `MessageTokensCount.RawJSON()` |
| 8 | MINOR | e2e/anthropic_sdk_test.go | ThinkingBlock test asserted 4xx + 0 dispatch only | additionally pins the A13 shape via `anthropic.Error.Type()` == `invalid_request_error` |
| 9 | MINOR | e2e/openai_sdk_test.go | Responses streaming ignored the golden stream's tool_calls | asserts the full function_call translation: output_item.added (type function_call, call_id call_abc123, name get_weather) + `function_call_arguments.delta` fragments assembled |
| 10 | MINOR | smoke/smoke_test.go | CodexBackendShape failure message assumed shape mismatch for any error | differentiates via `provider.UpstreamError`: HTTP status present → live backend answered → shape hypothesis falsified (escalate); absent → transport/credential failure, not a shape finding |

One incidental catch while implementing finding 9: the first test revision read
the arguments fragment from the wrong union field (`Arguments` instead of
`Delta`); verified against the SDK's `ResponseFunctionCallArgumentsDeltaEvent`
(JSON field `delta`) — test defect, encoder correct.

## 5. Phase 1 DoD checklist

- [x] OpenAI httpapi surface incl. chat/completions, models (dual-shape), responses
- [x] Anthropic httpapi surface incl. messages, count_tokens, models discovery
- [x] IR + codecs with golden-fixture contract tests (fixtures first, RED-GREEN)
- [x] SSE state machines + mock-provider streaming harness (real HTTP, scripted SSE)
- [x] Routing engine + health tracker + route_trace + per-model compat policies
      (§6.4: thinking strip/map/block, never-silent compat_policy events)
- [x] Codex OAuth provider (browser + device flow, encrypted token store,
      server-side refresh)
- [x] Ollama Cloud provider (native `/api/chat`)
- [x] Custom OpenAI-compat provider
- [x] Firecrawl v2 client + web_search/web_fetch emulation (§6.2)
- [x] Official OpenAI SDK client runs end-to-end against the proxy (mock in CI)
- [x] Claude Code path (official Anthropic SDK) runs end-to-end against the proxy
      (mock in CI)
- [x] Claude Code → non-Claude model path with route_trace showing thinking
      handling (test #10, §2)
- [x] Live smoke ENV-gated (never default CI) pinning OP-3/OP-4
- [x] Independent review pass completed, blocking findings resolved (§4)
- [x] Verification report archived (this document)

## 6. Budget numbers

Performance budgets (§8: proxy-added latency p99 ≤ 10 ms, first-byte overhead
≤ 15 ms, RSS ≤ 128 MB @ 100 streams) are verified by the Phase 3 release-gate
benchmark run; Phase 1 ships the paths they measure. WebUI bundle budget is
Phase 2. No performance-regression-relevant scope regressed in Phase 1 (no
benchmark baseline existed pre-Phase 1).

## 7. Gate decision

**PASSED.** Phase 1 DoD (plan §11) fully met: official OpenAI SDK client and the
Anthropic SDK (the Claude Code gateway path) run end-to-end against the proxy
against the scripted mock (12/12), including the Claude Code → non-Claude model
path with route_trace showing thinking handling (§2 test #10); live smokes are
ENV-gated and skip by default (OP-3/OP-4 pinned by smoke, live run
operator-gated); the independent review pass completed with verdict PASS, all
10 findings resolved in `2d5f1c8`, no blocking findings outstanding; all four
local gates green (§1) and the CI workflow runs the remaining gates
(-race, govulncheck, gitleaks, docker). This report is the phase-gate artifact —
Phase 1 is closed. Phase 2 (WebUI) begins with its mandatory live, cited
best-practice research pass per Analysis F doctrine.