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
- [ ] Green run: `go test -race ./...` output pasted

## Review quality gates

- [ ] No dead methods / unused imports
- [ ] No log call site bypasses `internal/observability/redact`
- [ ] No post-mutation logging
- [ ] No race regressions (locks removed only with replacement guarantee)
- [ ] No snapshot-of-our-output tests used as spec

## WebUI changes (if any)

- [ ] Visual regression baselines updated with reviewed screenshot diff
- [ ] Bundle budget ≤ 200 KB gzip initial load