# Contributing to PROxyLLM

Thank you for contributing! This project has strict quality gates — they
exist because a proxy's correctness is protocol fidelity.

## Ground rules

- **English** everywhere in the repo (code, docs, commits, issues).
- MIT license; by contributing you agree your work is licensed under it.

## Test doctrine (binding)

1. **Tests derive from specifications, not from code.** A test that only
   reproduces what the implementation does is a defect. Wire-protocol shapes
   come from `testdata/golden/` fixtures annotated with source URL + access
   date (see testdata/golden/README.md).
2. **RED-GREEN**: write the failing test first (from fixture/spec), then
   make it green in code. Tests change only when demonstrably misreading the
   spec — document that in the PR.
3. **Streaming changes** require race-safety: `go test -race ./...` must
   pass; cancellation and backpressure paths need explicit coverage.

## Review quality gates

PRs are reviewed against this checklist — reviewers reject on:

- Dead methods / unused imports
- Logging that bypasses `internal/observability/redact` (any log call site
  that could emit secrets)
- Post-mutation logging (logging state after it was changed — log the old
  state or log the transition)
- Race regressions (removing a lock/guard without replacing its guarantee)
- Snapshot tests of proxy output used as spec source (circular reasoning)

## Verification requirements per phase

- Preceding **best-practice research pass** (live, cited) for any new
  domain.
- Closing **independent review** — no code merged by its author alone.
- **Visual regression tests** for WebUI changes (Playwright against the
  console's own rendered baselines).
- **Verification report** (tests run, findings, resolutions) archived with
  the phase.

## Development setup

```bash
make build          # go build -trimpath -o proxyllm ./cmd/proxyllm
make test           # go test $(GO_PKGS) — never ./..., see the Makefile note
make lint-tool      # once per machine: installs the golangci-lint CI pins
make lint           # golangci-lint
```

The Go package list is always the Makefile's `GO_PKGS` (`./cmd/... ./internal/...
./webui`). `./...` descends into `webui/node_modules` and breaks vet/test/lint —
use the targets, or the explicit list.

### Toolchain versions

Always the current, safest release — check before pinning or keeping one, and
check the dependents' peer ranges too, because "latest" is not "safest" when a
plugin cannot follow.

| Tool | Version | Note |
|---|---|---|
| Go | 1.27 (`go.mod`) | `go-version-file` in CI |
| Node / npm | 26 / 12 | `engines: node >=26`; CI and the workflows pin 26 |
| golangci-lint | `v2.14.0` | `GOLANGCI_VERSION` in the Makefile **and** `ci.yml` — bump together |
| GoReleaser | `v2.18.3` | pinned in `release.yml`: a release must be reproducible |

Deliberately held, with the blocking upstream condition — re-check, do not jump:

| Tool | Latest | Held at | Blocked by |
|---|---|---|---|
| ESLint | 10.12.0 | 9.39.5 | `eslint-plugin-react` 7.37.5 (the only release) declares `eslint: ^3 … ^9.7` |
| TypeScript | 7.0.2 | 6.0.3 | `@typescript-eslint` 8.71.1 declares `typescript: >=4.8.4 <6.1.0` |

## Commit style

Conventional commits (`feat:`, `fix:`, `docs:`, `test:`, `chore:`,
`refactor:`) — subject ≤ 72 chars, imperative mood.
