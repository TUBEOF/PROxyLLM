---
name: proxyllm-ops
description: Use before calling a PROxyLLM change done, when running the gates, CI, e2e or smoke tests, cutting a release or tagging a version, or preparing a pull request.
---

# PROxyLLM operations: gates, CI, release

`docs/development.md` explains each Makefile target; this is the checklist.

## Gates before "done"

```bash
make fmt vet lint && make test
make test-race                       # anything streaming-adjacent
make check-tests                     # refuses deleted/weakened/skipped tests
make webui-lint webui-typecheck      # when webui/src changed
```

Packages are always `./cmd/... ./internal/... ./webui` — never `./...`
(descends into `webui/node_modules`). `golangci-lint` is pinned in the
Makefile AND `ci.yml`; bump both together.

## Tests that never run in CI

- Live smoke: `PROXYLLM_SMOKE=1 go test ./internal/smoke/...`
- Playwright screenshot baselines: rendered in CI only; the assertion suite
  runs locally with `--project=desktop-chrome`.

## Pull requests

- Conventional commits, subject ≤ 72 chars, imperative, English.
- Doc pages a change touches land in the same change.
- Workflow-file changes (`.github/workflows/*`) need a token with `workflow`
  scope; a stacked PR's base deletion closes its children — never rebase away
  a base branch with open children.
- CI can flake on the Docker-Hub-dependent jobs (setup-buildx pull timeout):
  retry before treating it as red.

## Release

1. Everything merged to `main`, gates green locally.
2. Tag `vX.Y.Z` and push it; watch the release workflow (Docker Hub flake:
   retry until green before announcing).
3. Once the image is pullable, land the version-pin commit exactly as
   `docs/development.md#release` prescribes — every reference it lists,
   including `docker/compose.yaml` and `SECURITY.md`. Never pin a tag the
   registry cannot serve yet.
4. A `route_trace` or wire change without its pinning tests is not releasable.