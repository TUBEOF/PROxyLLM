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
- **Visual regression tests** for WebUI changes (Playwright vs UpGuard kit
  baselines).
- **Verification report** (tests run, findings, resolutions) archived with
  the phase.

## Development setup

```bash
make build          # go build -trimpath -o proxyllm ./cmd/proxyllm
make test           # go test $(GO_PKGS) — never ./..., see the Makefile note
make lint           # golangci-lint
```

The Go package list is always the Makefile's `GO_PKGS` (`./cmd/... ./internal/...
./webui`). `./...` descends into `webui/node_modules` and breaks vet/test/lint —
use the targets, or the explicit list.

## Commit style

Conventional commits (`feat:`, `fix:`, `docs:`, `test:`, `chore:`,
`refactor:`) — subject ≤ 72 chars, imperative mood.
