# Development

How to build, test and release PROxyLLM. Process expectations (what a pull
request must contain, commit style) live in
[CONTRIBUTING.md](../CONTRIBUTING.md); this page is the mechanics.

## Toolchain

| Tool | Version | Where it is pinned |
|---|---|---|
| Go | see `go.mod` | `go.mod`, `ci.yml` (`go-version-file`) |
| Node | 26 | `webui/package.json` (`engines`), `ci.yml` |
| golangci-lint | see `GOLANGCI_VERSION` in the `Makefile` | `Makefile` **and** `ci.yml` — bump both together or not at all |

A locally installed linter older than the pin does not merely report
different findings: it refuses to load a module that requires a newer Go.
`make lint-tool` installs the pinned release into `$(go env GOPATH)/bin`.

The frontend toolchain is deliberately held back in two places — ESLint by
its React plugin's peer range and TypeScript by the typescript-eslint range
— so an upgrade waits for the plugin, not the other way round. Bumping them
alone breaks the lint run; bump the plugin first.

## Build and run

```bash
make build        # go build -trimpath -o proxyllm ./cmd/proxyllm
make run          # build and run with the repo's data/ directory
make docker       # build the image from source and run the compose file
```

WebUI (React + Vite + TypeScript, embedded into the binary):

```bash
make webui-install webui-build   # npm ci && npm run build
make build                       # then rebuild the binary to embed dist/
```

## Packages and the `./...` rule

Go commands are **always scoped** to the module's own packages:

```
./cmd/... ./internal/... ./webui
```

Never `./...`: some npm packages ship Go sources, so `./...` descends into
`webui/node_modules` and breaks vet, test and lint. The `Makefile` exports
the list as `GO_PKGS` and CI uses the same value.

## Tests

```bash
make test                     # the suite
make test-race                # required for anything streaming-adjacent
make vet                      # go vet on the scoped packages
make fmt                      # go fmt on the scoped packages
make lint                     # golangci-lint (see the pin above)
make check-tests              # guards the suite against erosion
go test ./internal/routing -run TestResolveChain -v   # one package, one test
```

`make check-tests` compares against the base branch (`-base origin/main` by
default) and fails on a deleted test, a weakened one, a new skip or a
tautology. It reports — without failing — assertions that merely re-encode
a production condition.

### The doctrine behind them

Tests derive from **specifications**, never from this implementation's
output: annotated golden fixtures under `testdata/golden/`, each carrying
the URL it was read from and the date, with contract tests written against
them. A test that only reproduces current behaviour is a defect, and a
snapshot of the proxy's own output proves nothing.

To add a wire shape: read the reference, write the fixture plus its
`.meta.json`, write the failing test, then the codec. When a reference
changes, the fixture and its date change **first**.

Streaming changes get a race run, and the streaming harness drives scripted
SSE — fragmented deltas, interleaved tool calls, pings, mid-stream errors,
unknown event types — through the real HTTP path rather than a mock of it.

## WebUI development

```bash
cd webui && npm run dev        # dev server against a running proxy
make webui-lint webui-typecheck
```

The design system is not optional: `design/DESIGN.md` states the rules,
`design/colors_and_type.css` is the token source (copied verbatim to
`webui/src/styles/colors_and_type.css` — a Go test keeps the copies
identical), and page code references tokens, never colour literals. The
design-rule assertions enforce it: dark-only, tokens-only, no emoji.

Two rules have broken releases before, so they are checked on every
Playwright run locally:

- **Never put variable-length text in a flex-wrap line above other
  content.** Font-metric drift between machines moves the wrap point and
  shifts everything below it by a line — use block layout.
- **`useMutation().error` read right after `run()` returns is the
  *previous* mutation's error.** Branch on the thrown error or on `run()`'s
  null return.

### The suites

| Suite | Runs where | What it covers |
|---|---|---|
| `e2e/design-rules.spec.ts`, `e2e/log-childops.spec.ts`, `e2e/provider-type-form.spec.ts`, `e2e/setup.spec.ts` | anywhere | Design rules, log rendering, provider forms, first-run wizard — no image baselines |
| `e2e/routes.spec.ts` and the screenshot suites | CI | Multi-viewport screenshots against committed baselines |

Screenshot baselines are rendered **in CI only**; a local render differs by
font rasterization. Refresh them with the `update-visual-baselines`
workflow, never by hand. The suite needs the fixture *binary* rebuilt, not
just the SPA, because the fixture server embeds `webui/dist`:

```bash
go build -o webui/e2e/fixturesrv-bin ./webui/e2e/fixturesrv
cd webui && npx playwright test -c playwright.config.ts \
  e2e/design-rules.spec.ts e2e/log-childops.spec.ts e2e/provider-type-form.spec.ts
```

The initial bundle has a **200 KB gzip budget**, enforced in CI. The Logs
view is code-split, so it does not count against the initial load.

## Smoke tests

Live against real upstreams, gated by an environment variable, and never in
CI:

```bash
PROXYLLM_SMOKE=1 go test ./internal/smoke/...
```

They read their targets from the environment:
`PROXYLLM_SMOKE_OLLAMA_{URL,MODEL,KEY}` for a local Ollama,
`PROXYLLM_SMOKE_OLLAMA_CLOUD_{URL,MODEL,KEY}` for the hosted one, and
`PROXYLLM_SMOKE_CODEX_{REFRESH_TOKEN,ISSUER,BACKEND}` for a Codex account.
They never assert on token values and never echo an upstream body.

### Local test instance

For a disposable instance with seeded providers, real client CLIs and a
machine-readable report, use the `proxyllm-test-instance` skill; the stable
procedure and invariants live in [Local test instance](test-instance.md).

```bash
.agents/skills/proxyllm-test-instance/scripts/test-instance.sh init \
  --home ~/proxyllm-test-instance --key-file ~/.config/proxyllm/keys.env
.agents/skills/proxyllm-test-instance/scripts/test-instance.sh start --build
.agents/skills/proxyllm-test-instance/scripts/test-instance.sh setup
.agents/skills/proxyllm-test-instance/scripts/test-instance.sh test
```

## Performance

The end-to-end budgets — proxy-added p99 latency, streaming first-byte
overhead, memory at 100 concurrent streams — live in
`internal/e2e/benchmarks_test.go` and are asserted against on a quiet
runner. `PROXYLLM_PERF_BUDGETS=report` logs the numbers instead of failing,
which is what the non-blocking CI benchmark job uses on `main`. The release
workflow runs the same test as a **hard gate**, so a regression blocks a
release rather than every pull request.

## Debugging

- `PROXYLLM_LOG_LEVEL=debug` adds the per-step detail (including the
  compat-policy decisions and emulation attempts); `PROXYLLM_LOG_FORMAT=text`
  is easier to read by eye.
- A request's `route_trace` explains what the router and executor did —
  event catalogue in [route trace](reference/route-trace.md). The console's
  Logs page renders the same data.
- Redaction is not optional: every log call goes through
  `internal/observability/redact`. If you add a log site that could emit a
  credential, it is a review rejection — not a follow-up.

## Release

A release is a tag. The workflow builds the SPA, runs GoReleaser (per-OS
archives plus a multi-arch container image), and publishes the GitHub
release; the perf-budget gate runs before any of it.

1. Merge everything that should ship to `main`, with CI green.
2. Tag `vX.Y.Z` (annotated or not — the workflow triggers on `v*`) and push
   the tag.
3. The workflow publishes `ghcr.io/tubeof/proxyllm:vX.Y.Z` and the archives.
4. Once the image is pullable, point the version references at the new tag —
   the pinned image default in `docker/compose.yaml`, the `docker run`
   example in [deployment](deployment.md), `info.version` in
   `docs/api/openapi.json`, and the supported-versions row in `SECURITY.md` —
   and commit that as
   `docs(release): point the pinned image default at vX.Y.Z`.

The pin moves after the image publishes, never with the tag: the tagged
tree keeps pointing at the previous release's image, so a fresh
`docker compose up` from any checkout always names a tag the registry can
serve. Pinning first has stranded deployments before — a publish job that
GitHub's billing check refused left the default image unpullable.

Never commit a built `webui/dist/index.html`: it is a placeholder that lets
a fresh clone compile without Node. `make webui-build` overwrites the
working copy only — restore it with `git checkout webui/dist/index.html`
before committing anything else.

## Verifying the docs

This documentation is part of the change, not a follow-up: a behaviour
change updates the page that describes it in the same commit. When you
touch a page, check that its commands still exist (`make …` targets), its
links resolve, and any number it quotes still matches the code — the
version in [route trace](reference/route-trace.md), the environment
variables in [configuration](configuration.md), and the route list in the
[API reference](api/README.md).
