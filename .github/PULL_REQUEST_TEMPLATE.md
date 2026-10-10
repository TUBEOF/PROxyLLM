## Summary

<!-- What does this PR do, and why? -->

## Spec provenance

<!-- For wire-protocol behavior: which golden fixture / official doc
     section does this implement? Link the fixture file and quote the
     relevant line if non-obvious. -->

- Fixture/doc:

## Test plan (RED-GREEN)

<!-- Evidence that tests were written first and failed before the fix:
     paste the red output (or describe the red run), then the green run. -->

- [ ] Tests written from spec/fixture, not from implementation behavior
- [ ] Red run shown
- [ ] Green run: `go test -race ./cmd/... ./internal/... ./webui` output pasted

## Review quality gates

- [ ] No dead methods / unused imports
- [ ] No log call site bypasses `internal/observability/redact`
- [ ] No post-mutation logging
- [ ] No race regressions (locks removed only with replacement guarantee)
- [ ] No snapshot-of-our-output tests used as spec

## WebUI changes (if any)

- [ ] `visual-regression` assertions green (they block; the screenshot
      comparison only reports — read its diff artifact and sticky comment)
- [ ] Baselines refreshed via the `update-visual-baselines` workflow (set its
      `branch` input to this branch) if the diff is a deliberate design change
- [ ] Bundle budget ≤ 200 KB gzip initial load
