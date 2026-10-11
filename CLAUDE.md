# CLAUDE.md

Guidance for Claude Code when working in this repository. Every statement here
was verified by running the command in question in this environment — when a
claim looks like it should be checked again, check it rather than trusting the
text.

## What this is

PROxyLLM is a single-binary LLM proxy in Go. It exposes two client-facing API
surfaces — OpenAI-compatible (`/v1/chat/completions`, `/v1/responses`,
`/v1/models`) and Anthropic-compatible (`/v1/messages`,
`/v1/messages/count_tokens`) — with a full bidirectional translation engine
between the two protocol families. Routing Claude Code to non-Claude models is a
first-class feature. One static CGO-free binary (embedded WebUI) plus one SQLite
data directory.

## Commands

Go packages are always scoped to `./cmd/... ./internal/... ./webui` — never
`./...`, which descends into `webui/node_modules` (some npm packages ship Go
sources). This is the `GO_PKGS` list in the Makefile and CI; keep them in parity.

```bash
make build            # go build -trimpath -o proxyllm ./cmd/proxyllm
make test             # go test $(GO_PKGS)
make test-race        # the CI gate for anything streaming-adjacent
make lint-tool        # once per machine: installs the golangci-lint CI pins
make lint             # golangci-lint run
make vet
make fmt              # gofmt -w $(GO_PKGS)
make run

# Single package / single test
go test ./internal/translate/...
go test ./internal/routing -run TestResolveChain -v

# WebUI (React + Vite + TS, embedded via webui/embed.go into dist/)
make webui-install webui-build   # npm ci && npm run build, then `make build` to embed
make webui-lint webui-typecheck

# Visual regression (Playwright). Baselines are CI-rendered; locally only the
# assertion-only spec runs green by design:
npx playwright install chromium webkit                     # once (see the note below)
go build -o webui/e2e/fixturesrv-bin ./webui/e2e/fixturesrv # the suite's webServer
cd webui && npx playwright test -c playwright.config.ts --project=desktop-chrome \
  e2e/design-rules.spec.ts e2e/log-childops.spec.ts         # assertions only

# Live smoke tests — never in default CI
PROXYLLM_SMOKE=1 go test ./internal/smoke/...
```

The binary also serves as its own container healthcheck: `proxyllm -healthcheck`
(distroless image has no curl).

## Toolchain

**Binding rule: always the current, safest version of every dependency and tool,
and consult the official docs before touching anything wire-shaped.** Check a
version against its current release *and* against its dependents' declared peer
ranges — "latest" is not "safest" when a plugin cannot follow.

Verified as of 2026-10-10 (`go version`, `node -v`, `npm outdated`,
`npm view <pkg> peerDependencies`, `gh api repos/<owner>/<repo>/releases/latest`):

| Tool | Version | Note |
|---|---|---|
| Go | 1.27.0 (`go.mod`) | CI uses `go-version-file` |
| Node / npm | 26 / 12 | workflows pin 26; `webui` `engines` follows |
| golangci-lint | v2.14.0 | `GOLANGCI_VERSION` in the Makefile **and** `ci.yml` — bump together, then `make lint-tool` |
| GoReleaser | v2.18.3 | pinned in `release.yml`: a release must be reproducible |
| Playwright | 1.64.0 | baselines are captured in CI, never locally |
| React / Vite / TS | 19.3.0 / 8.3.4 / 6.0.3 | |

Held deliberately, with the blocking upstream condition (re-check, do not jump a
major blind):

| Tool | Latest | Held at | Blocked by |
|---|---|---|---|
| ESLint | 10.x | 9.39.5 | `eslint-plugin-react` 7.37.5 (the only release) declares `eslint: ^3 … ^9.7` |
| TypeScript | 7.x | 6.0.3 | `@typescript-eslint` 8.71.1 declares `typescript: >=4.8.4 <6.1.0` |

A few of these land with the dependency PR rather than earlier in the stack
(Node 26 in the workflows and `engines`, `make lint-tool`, the GoReleaser pin) —
if you are reading this on a branch that predates it, those three read as 24 /
absent / `latest`, which is the state that PR fixes.

Toolchain gotchas verified here:

- A locally installed golangci-lint older than the CI pin **refuses to load this
  module at all** (`the Go language version used to build golangci-lint is lower
  than the targeted Go version`). `make lint-tool` is the fix.
- `shellcheck` is not installable from this environment's apt (no `universe`);
  install the release binary if you want actionlint's shell pass to run locally.

## Architecture

### Translation: canonical IR hub-and-spoke (ADR-004)

`internal/translate/ir` defines the canonical intermediate representation —
`Request`, `Response`, `StreamEvent` (start / block_start / deltas / block_stop /
message_delta / done / error / ping). Each protocol (`anthropic`, `openai`,
`ollama`) implements two codecs plus one streaming state machine per direction;
adding a protocol means one codec pair, never pairwise converters.
Non-representable wire fields ride in a per-request `passthrough` map —
re-emitted same-protocol, dropped with a debug log when crossing protocols. Tool
calls, thinking blocks (never fabricated signatures), and cache markers all have
documented per-field mappings in `docs/reference/translation-matrix.md`. SSE
handling (`internal/translate/sse`) uses bounded channels for backpressure and
propagates cancellation end-to-end.

**Wire facts come from `testdata/golden/`**, and each fixture's `.meta.json`
carries the source URL and the date it was read. Before changing anything
wire-shaped, re-read the current official docs and update that date — a fixture
encoding a shape the reference does not describe is a defect (ADR-006 makes this
a gate, not a preference).

### Routing: deterministic ordered rule model (ADR-005)

`internal/routing` resolves, per request: client-key scope → model match
(exact/wildcard/alias map for Claude Code aliases) → capability filter
(web_search, web_fetch, code_execution, streaming, thinking — derived from
request content; vision is NOT gated, and neither is the context window: a real
upstream rejection classifies as `capability_gap` (unsupported modality) or
`context_window_exceeded` (input longer than the model's window, ADR-013) and
fails over — when the whole chain failed on `capability_gap`, the executor walks
the configured fallback MODELS to **describe each image** (ADR-014: they receive
the image and the instruction, never the conversation, so their own context
window cannot disqualify them), substitutes the descriptions for the images, and
dispatches the routed chain again; `vision:false` is learned into the model row
unless pinned) → ordered fallback chain → health gate (circuit breaker, backoff,
probe recovery).

**Failover only before the first streamed byte** — no mid-stream provider swap;
an upstream failure during streaming terminates with a protocol-conform error
event. Every transition (skip, retry, failover, emulated-tool substitution,
compat-policy application) is recorded in `route_trace` in request logs and
rendered in the WebUI — fallbacks are never silent. The route_trace schema is
versioned because the WebUI renders it: **v7** adds `emulated_tool_attempt`
(v6 `context_override`, v5 `implicit_route`, v4 `model_resolved`, v3
`capability_fallback`). Bumps are additive-only and touch the two
version-pinning tests (`strategy_e2e_test.go`, `wiremodel_test.go`) plus the
comment block above `chainTrace`.

A sub-operation's own body (the tool exchange it answered, the caption it
produced) rides the Trace in memory and is stored in `request_payloads` under
`owner_kind = "child"` — deliberately NOT in `route_trace`, which stays metadata
and, unlike the payload table, is not redacted on write.

### Web-tool emulation (ADR-015)

`web_search`/`web_fetch` are served by an account pool
(`internal/routing/fallback.go`) whose backends are `internal/tools/firecrawl`
and `internal/tools/ollamaweb` (Ollama's hosted `/api/web_search` and
`/api/web_fetch`). The pool is backend-agnostic — an entry is an id plus the
two-method `Emulator`, with no backend-type field — so a new backend needs only
a catalog entry, a config parse case and a `buildProvider` case. Ollama Cloud
(Web Tools) is a *separate account type* from `ollama-cloud`, not a second role
for the chat account. Three outcomes per
account: **answered** (non-empty result) ends the walk, **empty** (a search with
no matches, a page the service could not serve) continues it, **failed** moves
on; an empty answer still outranks a later failure, because "nothing found" is
an answer and "the tool broke" is not. An empty *fetch* is an error **to the
client** (`url_not_accessible`, what the reference documents for a fetch that
could not retrieve content) even though the pool still walks past it — the walk
decides which account serves the call, the encoder decides what the client is
told. Three bounds keep a slow account from
holding the request: a per-invocation call budget (30 s, across all accounts), a
per-account circuit (three failures cool it down, and an all-cooling family is
still probed), and a 5-minute loop budget. Every attempt is an
`emulated_tool_attempt` event naming the account and its outcome.

A fetched page is bounded by the definition's `max_content_tokens` — documented
behaviour ("If the fetched content exceeds this limit, the tool truncates it"),
applied where the page is accepted so it covers both the model-facing payload and
the client's result block. Zero means no limit; the limit is text-only. An
unbounded page is what once pushed the upstream into an empty turn.

### Server wiring

`cmd/proxyllm` → `internal/server` (root handler; Option-pattern assembly:
`WithProvider`, `WithMasterKey`, `WithEmulator`) → `internal/server/httpapi`
(the API surfaces plus the admin API behind auth middleware) and
`internal/server/webui` (SPA serving). The master key doubles as the
session/CSRF HMAC secret, so WebUI sessions and at-rest secrets rotate together.
Features degrade explicitly, never silently: no web-tool account → web-tool
emulation returns documented "unavailable" errors; no master key → admin API
answers a uniform 401.

### Storage, auth, security (ADR-007, ADR-016)

`internal/storage` is SQLite (`modernc.org/sqlite`, CGO-free) — providers,
client keys, routing rules, sessions, request logs, usage, envelope-encrypted
secrets. Provider credentials are AES-256-GCM envelopes; master key from
`PROXYLLM_MASTER_KEY` or a generated 0600 keyfile. **Client keys are stored as
keyed MACs** (`$hmacsha256$v=1$…`, HMAC-SHA256 under a master-key-derived
secret) — display-once secrets, verified in microseconds; legacy Argon2id rows
keep verifying and are rewritten on first successful use. **Admin passwords keep
Argon2id** (they are low-entropy). Consequence to remember: rotating the master
key invalidates every client key.

`internal/auth` implements default-ON auth (WebUI login plus per-client API keys
on both surfaces); opt out only via an explicit `PROXYLLM_AUTH_DISABLED=1` with a
loud startup warning. **All logging goes through `internal/observability/redact`**
— masks Bearer tokens, `sk-`/`fc-`/`pk_` keys, JWTs, oversized base64. Any log
call site that could emit secrets bypassing the redactor is a review rejection.

The store runs a small connection pool (4) with WAL + `busy_timeout`; the live
log hub, the provider registry, the router's round-robin counter and the health
tracker are all lock-split so unrelated requests do not queue behind each other
(ADR-016 records what was measured and what was deliberately deferred).

### Providers & web-tool accounts

`internal/provider` defines the provider interface; built-ins: `codex`
(ChatGPT/Codex OAuth PKCE, 127.0.0.1-only callback), `ollamacloud` (native
`ollama.com/api/chat`), `custom` (arbitrary OpenAI-compatible endpoints, with
SSRF guards on base URLs). `internal/tools/firecrawl` and
`internal/tools/ollamaweb` are the web-tool backends — they emulate
web_search/web_fetch for providers lacking native server
tools, producing protocol-conform blocks. Outbound HTTP goes through
`internal/httpguard` (private-address guard, tuned transport).

### WebUI

React 19 + Vite + react-router, TypeScript strict, embedded into the binary via
`webui/embed.go`. Bundle budget 200 KB gzip initial load (CI-enforced). Visual
regression: Playwright, baselines captured in CI (ADR-006 V-3/V-4) — never
regenerate them locally.

- Never put variable-length text in a flex-wrap line above other content —
  font-metric drift between CI runner image rollouts tips the wrap point and
  shifts everything below by one line (broke v1.0.1 AND v1.0.2); use block
  layout.
- Reading `useMutation().error` right after `run()` sees the PREVIOUS mutation's
  error (stale state) — branch on the thrown error or `run()`'s null return.
- Changing rendered UI means the baselines will differ; refresh them with the
  `update-visual-baselines` workflow (see CI) rather than by hand.

## CI/CD

Four workflows under `.github/workflows/`, all pinned to `ubuntu-24.04`
(`ubuntu-latest` drifts, and the visual baselines are rendering-sensitive):

| Workflow | Trigger | What it does |
|---|---|---|
| `ci.yml` | push to main, every PR, manual | lint (gofmt/vet/golangci-lint), `go test -race` + govulncheck, gitleaks (+ CodeQL when the repo is public), WebUI build + bundle budget, actionlint, Docker build + smoke. `benchmark` is **main-only** and non-blocking |
| `visual-regression.yml` | PRs touching UI paths only (`webui/src`, `e2e`, `styles`, `package*.json`, the workflow itself), manual | Assertion suite (design rules + log behaviour) **blocks**; the screenshot comparison only **reports** — diff artifact plus one sticky PR comment, and the job exits 0 |
| `update-visual-baselines.yml` | manual, with a `branch` input | Regenerates the baselines in CI, commits them to that branch, then re-dispatches `ci.yml` with `gh workflow run` |
| `release.yml` | tag `v*` | `perf-budgets` (hard gate) → GoReleaser + GHCR |

Two things that used to be painful and no longer are:

- **No more "loop protection" empty commits.** A push made with `GITHUB_TOKEN`
  cannot create a workflow run, which is why a baseline refresh needed a
  maintainer to push a throwaway commit. `workflow_dispatch` (and
  `repository_dispatch`) are exempt, so the refresh workflow now dispatches
  `ci.yml` explicitly — and `ci.yml` declares the `workflow_dispatch` trigger for
  exactly that. **Caveat:** the re-dispatch works only once the workflow exists on
  the default branch, so the first refresh after this change must follow the merge
  to `main`.
- **A UI PR no longer goes red for a rendering difference it did not cause.** The
  comparison runs only for UI-touching PRs and never fails the check; the
  assertion suite (which has no baselines) is the blocking half.

Expected order for a deliberate UI change: open the PR (the comparison reports a
diff, green check), dispatch `update-visual-baselines` with `branch` = that
branch, pull the regenerated commit, and let the re-dispatched run confirm.

The absolute perf ceiling (`internal/e2e` `TestPerfBudgets`, 10 ms p99) needs a
quiet-runner recalibration pass: this container measures ~16-19 ms for code the
release runner historically measured at 2.6-5.9 ms. It runs in report mode on
main and hard in the release pipeline.

## Test doctrine (binding — ADR-006, CONTRIBUTING.md)

1. **Tests derive from specifications, not from code.** Wire shapes come from
   `testdata/golden/` fixtures annotated with source URL + access date. A test
   that only reproduces implementation behavior is a defect; snapshot tests of
   proxy output as spec source are rejected (circular).
2. **RED-GREEN**: failing test first (from fixture/spec), then green. Tests change
   only when demonstrably misreading the spec — documented in the PR.
3. **Streaming changes** need race safety and explicit coverage of cancellation +
   backpressure paths (`internal/testutil/mockprovider` scripts fragmented SSE,
   interleaved tool calls, pings, mid-stream errors over real HTTP).
4. Review gates also reject: dead methods, unused imports, logging bypassing the
   redactor, post-mutation logging (log the old state or the transition), race
   regressions from removed locks.

Performance claims get the same treatment: a latency assertion that would only
re-measure the host belongs in a *relative* form (this path against that path) or
in report mode. `internal/e2e/scalability_test.go` is the pattern.

## Change discipline (binding, user-set 2026-09-21)

1. **Impact analysis before every change**: before editing a function, state
   which other files are affected (callers, wire contracts, tests, WebUI
   renderers, route_trace schema). Cross-file regressions from blind edits have
   burned past sessions. Use codegraph blast radius or an equivalent call-path
   check first.
2. **Security review before every push**: a thorough security review (diff review
   with the review crew / security-review skill) gates every push. Local commits
   are always allowed and encouraged per change; only pushing waits for the green
   review. Checklist: redactor coverage at every new log site, secrets never in
   config blobs, SSRF guards on any new outbound URL path, session/CSRF gating on
   new admin endpoints, no live calls or wall-clock state in fixturesrv.
3. **Docs are part of the deliverable**: `docs/` (getting-started,
   configuration, api-reference, routing-and-fallbacks, security, deployment)
   plus the ADRs under `docs/adr/` (001–016) stay consistent with the behavior.

## Conventions

- English everywhere (code, docs, commits). Conventional commits (`feat:`,
  `fix:`, `docs:`, `test:`, `chore:`, `refactor:`), subject ≤ 72 chars,
  imperative.
- Mapping tables for vendor-specific wire facts (e.g. server-tool capability
  strings) live in configuration, not hardcoded constants — vendors rotate them.
- `.editorconfig` requires LF and a final newline; `.gitattributes` is LF except
  a few Windows-script patterns that this repository does not contain.
