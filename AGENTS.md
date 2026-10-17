# AGENTS.md

PROxyLLM is a single-binary LLM proxy in Go. One process serves three client
surfaces — OpenAI-compatible (`/v1/chat/completions`, `/v1/responses`,
`/v1/models`), Anthropic-compatible (`/v1/messages`,
`/v1/messages/count_tokens`) and decision models (`/v1/systemone`,
`/v1/decisions`) — with a canonical IR between the protocol families, so
Claude Code can run against non-Claude models.

Depth lives in `docs/`, indexed by `docs/README.md`. Read the page a change
touches; do not restate it here.

## Commands

Go packages are always scoped to `./cmd/... ./internal/... ./webui` — **never
`./...`**, which descends into `webui/node_modules` and breaks vet, test and
lint. The `Makefile` is the source of truth; `docs/development.md` explains
each target.

    make build          # go build -trimpath -o proxyllm ./cmd/proxyllm
    make test           # go test $(GO_PKGS)
    make test-race      # the gate for anything streaming-adjacent
    make vet
    make fmt
    make lint           # golangci-lint (see the pin below)
    make check-tests    # refuses deleted, weakened or newly skipped tests
    make run

    go test ./internal/translate/...                     # one package
    go test ./internal/routing -run TestResolveChain -v  # one test

`golangci-lint` is pinned in the `Makefile` **and** in `ci.yml`; bump both
together. If the local binary is older than the pin it will refuse to load
the module — `make lint-tool` installs the pinned release.

WebUI (React + Vite + TypeScript, embedded into the binary):

    make webui-install webui-build   # npm ci && npm run build, then `make build` to embed
    make webui-lint
    make webui-typecheck

The Playwright visual baselines are rendered in CI, never locally. The
assertion suite has no baselines and runs anywhere. Rebuild the fixture
**binary**, not just the SPA, before running it — it embeds `webui/dist`:

    go build -o webui/e2e/fixturesrv-bin ./webui/e2e/fixturesrv
    cd webui && npx playwright test -c playwright.config.ts \
      e2e/design-rules.spec.ts e2e/log-childops.spec.ts e2e/provider-type-form.spec.ts

Live smoke tests exist and never run in CI: `PROXYLLM_SMOKE=1 go test
./internal/smoke/...`.

## Layout

- `internal/translate/ir` — the canonical intermediate representation. Every
  protocol is a codec pair plus one streaming state machine per direction
  against this hub; there are no pairwise converters, so adding a protocol
  means one codec pair.
- `internal/translate/{anthropic,openai,ollama,systemone}` — those codecs.
  `sse` is the shared event-stream reader and writer.
- `internal/routing` — resolves each request through an ordered rule chain and
  records every transition in `route_trace`, which the console renders and
  whose schema is therefore versioned.
- `internal/storage` — SQLite (`modernc.org/sqlite`, CGO-free), with migrations
  as an append-only list.
- `internal/provider`, `internal/tools` — upstream providers and the web-tool
  backends.
- `internal/server`, `internal/server/httpapi`, `internal/server/webui` —
  wiring, the API surfaces, the admin API and the embedded SPA.
- `testdata/golden/` — wire fixtures. Each `.meta.json` carries the URL it was
  read from and the date.
- `design/` — the UI's design system: `DESIGN.md` for the rules,
  `brand.json` for the machine-readable profile, `colors_and_type.css` for
  the token values. The skill under `.claude/skills/proxyllm-design` loads on
  its own when a task touches `webui/`; read `design/DESIGN.md` before
  inventing a colour, spacing or component.
- `docs/reference/translation-matrix.md` — the per-field mapping between
  protocols.

## Conventions that differ from the default

- Tests derive from the golden fixtures and from the specifications, never
  from this implementation's own output. A test that only reproduces current
  behaviour is a defect.
- Changing anything wire-shaped starts by re-reading the current official
  documentation and updating that fixture's source and date. A fixture
  encoding a shape the reference does not describe is a bug, not a snapshot.
- Every log call goes through `internal/observability/redact`. Bearer tokens,
  `sk-`/`fc-`/`pk_` keys, JWTs and oversized base64 are masked there. A log
  site that could emit a secret around it is a review rejection.
- Vendor-specific wire facts (capability strings, error codes) live in
  configuration, not in constants — vendors rotate them.
- `route_trace` schema changes are additive and land together with the two
  tests that pin the version.
- Conventional commits (`feat:`, `fix:`, `docs:`, `test:`, `chore:`,
  `refactor:`), subject ≤ 72 characters, imperative, English everywhere.

## WebUI rules that have broken releases before

- Never put variable-length text in a flex-wrap line above other content.
  Font-metric drift between runner images moves the wrap point and shifts
  everything below it by a line. Use block layout.
- Reading `useMutation().error` right after `run()` returns the *previous*
  mutation's error. Branch on the thrown error or on `run()`'s null return.
- Changing rendered UI changes the visual baselines. Refresh them with the
  `update-visual-baselines` workflow rather than by hand.
- The bundle budget is 200 KB gzip for the initial load, enforced in CI. The
  Logs view is lazily loaded, so it does not count against it.

## Gates before calling a change done

- `make fmt && make vet && make lint`, then `make test` — plus `make test-race`
  when anything streaming changed.
- Touched `webui/src`: `make webui-lint webui-typecheck`.
- Touched a wire shape: update `docs/reference/translation-matrix.md` in the
  same change.
- Changed behaviour that `docs/` describes: update the doc in the same change.

## Do not

- Do not run `go test ./...` or `golangci-lint run ./...`.
- Do not commit a built `webui/dist/index.html`. It is a committed placeholder
  that lets a fresh clone build without a Node toolchain, and `make webui-build`
  overwrites it with the real bundle — so run `git checkout webui/dist/index.html`
  before committing, and check `git diff origin/main -- webui/dist/index.html`
  is empty.
- Do not commit secrets, tokens or `.env` contents. The master key never
  appears in a config blob.
- Do not reformat or rename files outside the change's scope.
- Do not move guidance out of `docs/` into this file to make it easier to find.

## Where the depth lives

| Question | Read |
|---|---|
| Why is it built this way? | `docs/adr/` — one file per decision, with a stable `ADR-NNN` identifier the source comments cite |
| How does field X of protocol A map to B? | `docs/reference/translation-matrix.md` |
| What does a compatibility setting do? | `docs/reference/model-compat.md` |
| What is in a `route_trace`? | `docs/reference/route-trace.md` |
| Endpoints, request/response shapes, errors | `docs/api/` (start at `README.md`) |
| Configuration keys and layers | `docs/configuration.md` |
| The console's pages | `docs/operating.md` |
| Routing, fallbacks, health | `docs/routing-and-fallbacks.md` |
| Building, testing, releasing | `docs/development.md` |
| Claude Code against non-Claude models | `docs/claude-code.md` |
| Security posture and threat model | `docs/security.md`, `SECURITY.md` |
| Deployment, container, sizing | `docs/deployment.md` |
