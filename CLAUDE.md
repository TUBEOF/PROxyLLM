# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

PROxyLLM is a single-binary LLM proxy in Go that exposes two client-facing API surfaces — OpenAI-compatible (`/v1/chat/completions`, `/v1/responses`, `/v1/models`) and Anthropic-compatible (`/v1/messages`, `/v1/messages/count_tokens`) — with a full bidirectional translation engine between the two protocol families. Routing Claude Code to non-Claude models is a first-class feature. One static CGO-free binary (embedded WebUI) + one SQLite data directory.

## Commands

Go packages are always scoped to `./cmd/... ./internal/... ./webui` — never `./...`, which descends into `webui/node_modules` (some npm packages ship Go sources). This is the `GO_PKGS` list in the Makefile and CI; keep them in parity.

```bash
make build            # go build -trimpath -o proxyllm ./cmd/proxyllm
make test             # go test $(GO_PKGS)
make lint             # golangci-lint run (gosec included; use //nolint:gosec with reason, never bare suppressions)
make vet
make fmt              # gofmt -w $(GO_PKGS)
make run

# Single package / single test
go test ./internal/translate/...
go test ./internal/routing -run TestRouterChainResolution -v

# Local Linux toolchain (Ubuntu 26.04): make/gcc are available, so the
# targets above run as-is; golangci-lint is at $(go env GOPATH)/bin/golangci-lint.
# `go test -race` works locally; the CI gate additionally enforces it.

# Streaming-related changes: -race must pass (locally and as the CI gate)
make test-race

# Deterministic assemblies: the visual-regression fixture server
# (webui/e2e/fixturesrv) must never make live upstream calls or persist
# wall-clock state — background jobs in server.New are opt-in options
# (pattern: WithModelSync); never gate them on the master key alone.

# Live smoke tests — never in default CI
PROXYLLM_SMOKE=1 go test ./internal/smoke/...

# WebUI (React 18 + Vite + TS, embedded via webui/embed.go into dist/)
make webui-install webui-build   # npm ci && npm run build, then `make build` to embed
make webui-lint webui-typecheck

# Visual regression (Playwright) — baselines captured in CI only, never locally.
# Local one-time setup (Ubuntu 26.04 is newer than Playwright 1.55 supports):
#   PLAYWRIGHT_HOST_PLATFORM_OVERRIDE=ubuntu24.04-x64 npx playwright install chromium webkit
#   # if the download stalls during extraction, unzip manually:
#   unzip /tmp/playwright-download-<browser>-ubuntu24.04-x64-1187.zip \
#     -d ~/.cache/ms-playwright/<browser>-1187
# Build the fixture server first (the webServer config expects it):
#   go build -o webui/e2e/fixturesrv-bin ./webui/e2e/fixturesrv
# Locally green by design: the assertion-only suite (no baselines involved):
cd webui && npx playwright test -c playwright.config.ts --project=desktop-chrome e2e/design-rules.spec.ts
# Full screenshot suites (fail locally by design — baselines are CI-rendered):
cd webui && npx playwright test -c playwright.config.ts         # seeded
cd webui && npx playwright test -c playwright.empty.config.ts   # empty states
```

The binary also serves as its own container healthcheck: `proxyllm -healthcheck` (distroless image has no curl).

CI (`.github/workflows/ci.yml`) gates: gofmt/vet/golangci-lint, `go test -race` + govulncheck, gitleaks/CodeQL, WebUI build + bundle budget (≤ 200 KB gzip initial load) + Playwright visual regression, Docker build + smoke. Perf budgets (`internal/e2e` `TestPerfBudgets`) run on main but only block release, not PRs. Expected CI order after a rendered-UI change: the push BEFORE the baseline regen fails visual regression (old baselines) — that red run is expected; green comes from the loop-protection commit after pulling the regen commit.

## Architecture

### Translation: canonical IR hub-and-spoke (ADR-004)

`internal/translate/ir` defines the canonical intermediate representation — `Request`, `Response`, `StreamEvent` (start / block_start / deltas / block_stop / message_delta / done / error / ping). Each protocol (`anthropic`, `openai`, `ollama`) implements two codecs + one streaming state machine per direction; adding a protocol means one codec pair, never pairwise converters. Non-representable wire fields ride in a per-request `passthrough` map — re-emitted same-protocol, dropped with a debug log when crossing protocols. Tool calls, thinking blocks (never fabricated signatures), and cache markers all have documented per-field mappings in `docs/reference/translation-matrix.md`. SSE handling (`internal/translate/sse`) uses bounded channels for backpressure and propagates cancellation end-to-end.

### Routing: deterministic ordered rule model (ADR-005)

`internal/routing` resolves, per request: client-key scope → model match (exact/wildcard/alias map for Claude Code aliases) → capability filter (web_search, web_fetch, code_execution, streaming, thinking — derived from request content; vision is NOT gated, and neither is the context window: a real upstream rejection classifies as `capability_gap` (unsupported modality) or `context_window_exceeded` (input longer than the model's window, ADR-013) and fails over — when the whole chain failed on `capability_gap` the executor walks the configured fallback MODELS, skipping any whose declared `context_window` cannot hold the request, and learns `vision:false` into the model row unless pinned) → ordered fallback chain → health gate (circuit breaker, backoff, probe recovery). **Failover only before the first streamed byte** — no mid-stream provider swap; an upstream failure during streaming terminates with a protocol-conform error event. Every transition (skip, retry, failover, emulated-tool substitution, compat-policy application) is recorded in `route_trace` in request logs and rendered in the WebUI — fallbacks are never silent. The route_trace schema is versioned because the WebUI renders it. Route-trace version bumps (`routeTraceVersion` in httpapi.go) are additive-only and touch the two version-pinning tests (`strategy_e2e_test.go`, `wiremodel_test.go`) plus the comment block above `chainTrace`.

### Server wiring

`cmd/proxyllm` → `internal/server` (root handler; Option-pattern assembly: `WithProvider`, `WithMasterKey`, `WithEmulator`) → `internal/server/httpapi` (the API surfaces + admin API behind auth middleware) and `internal/server/webui` (SPA serving). The master key doubles as the session/CSRF HMAC secret, so WebUI sessions and at-rest secrets rotate together. Features degrade explicitly, never silently: no Firecrawl key → web-tool emulation returns documented "unavailable" errors; no master key → admin API answers uniform 401.

### Storage, auth, security (ADR-007)

`internal/storage` is SQLite (`modernc.org/sqlite`, CGO-free) — providers, client keys, routing rules, sessions, request logs, usage, envelope-encrypted secrets. Provider credentials are AES-256-GCM envelopes; master key from `PROXYLLM_MASTER_KEY` or a generated 0600 keyfile; client keys stored as Argon2id hashes only (display-once). `internal/auth` implements default-ON auth (WebUI login + per-client API keys on both surfaces); opt-out only via explicit `PROXYLLM_AUTH_DISABLED=1` with a loud startup warning. **All logging goes through `internal/observability/redact`** — masks Bearer tokens, `sk-`/`fc-`/`pk_` keys, JWTs, oversized base64. Any log call site that could emit secrets bypassing the redactor is a review rejection.

### Providers & web-tool emulation

`internal/provider` defines the provider interface; built-ins: `codex` (ChatGPT/Codex OAuth PKCE, 127.0.0.1-only callback), `ollamacloud` (native `ollama.com/api/chat`), `custom` (arbitrary OpenAI-compatible endpoints, with SSRF guards on base URLs). `internal/tools/firecrawl` is the universal web-tool fallback — emulates web_search/web_fetch for providers lacking native server tools, producing protocol-conform blocks.

### WebUI

React 18 + Vite + react-router, TypeScript strict, embedded into the binary via `webui/embed.go`. Bundle budget 200 KB gzip initial load (CI-enforced). Visual regression via Playwright with baselines captured in CI (V-3/V-4 in ADR-006) — never regenerate baselines locally.

- Never put variable-length text in a flex-wrap line above other content — font-metric drift between CI runner image rollouts tips the wrap point and shifts everything below by one line (broke v1.0.1 AND v1.0.2); use block layout.
- Reading `useMutation().error` right after `run()` sees the PREVIOUS mutation's error (stale state) — branch on the thrown error or `run()`'s null return instead.
- Changing rendered UI requires a CI baseline regen before tagging a release: dispatch `update-visual-baselines.yml`, push an empty commit (GitHub loop protection), wait for CI green, then tag.

## Test doctrine (binding — ADR-006, CONTRIBUTING.md)

1. **Tests derive from specifications, not from code.** Wire shapes come from `testdata/golden/` fixtures annotated with source URL + access date. A test that only reproduces implementation behavior is a defect; snapshot tests of proxy output as spec source are rejected (circular).
2. **RED-GREEN**: failing test first (from fixture/spec), then green. Tests change only when demonstrably misreading the spec — documented in the PR.
3. **Streaming changes** need race safety and explicit coverage of cancellation + backpressure paths (`internal/testutil/mockprovider` scripts fragmented SSE, interleaved tool calls, pings, mid-stream errors over real HTTP).
4. Review gates also reject: dead methods, unused imports, logging bypassing the redactor, post-mutation logging (log the old state or the transition), race regressions from removed locks.

## Conventions

- English everywhere (code, docs, commits). Conventional commits (`feat:`, `fix:`, `docs:`, `test:`, `chore:`, `refactor:`), subject ≤ 72 chars, imperative.
- Mapping tables for vendor-specific wire facts (e.g. server-tool capability strings) live in configuration, not hardcoded constants — vendors rotate them.
- Docs under `docs/` (getting-started, configuration, api-reference, routing-and-fallbacks, security, deployment) are part of the deliverable; keep them consistent when changing behavior. ADRs under `docs/adr/` record the binding architectural decisions.

## Change discipline (binding, user-set 2026-09-21)

1. **Impact analysis before every change**: before editing a function, state which other files are affected (callers, wire contracts, tests, WebUI renderers, route_trace schema). Cross-file regressions from blind edits have burned past sessions. Use codegraph blast radius or an equivalent call-path check first.
2. **Security review before every push**: a thorough security review (diff review with the review crew / security-review skill) gates every push. Local commits are always allowed and encouraged per change; only pushing waits for the green review. Checklist: redactor coverage at every new log site, secrets never in config blobs, SSRF guards on any new outbound URL path, session/CSRF gating on new admin endpoints, no live calls or wall-clock state in fixturesrv.