# Contributing to PROxyLLM

Thanks for considering a contribution. This project keeps strict quality
gates, for one reason: a proxy's correctness is protocol fidelity, and a
wrong wire shape is worse than a missing feature.

## Ground rules

- **English** everywhere — code, comments, docs, commits, issues.
- MIT licensed; by contributing you agree your work ships under it.
- Discuss a large change before writing it. A short proposal (what, why,
  what it costs) in an issue saves a rewrite.

## Ways to contribute

| Kind | Notes |
|---|---|
| Bug reports | The most useful report names what you sent, what you expected and what happened — plus the request's `route_trace` if you have it |
| Provider requests | A new provider is a new integration: open an issue first, since dated wire facts need sources |
| Documentation | Corrections land fastest; a doc change ships with the behaviour change it describes |
| Code | See the gates below |

## Before you write code

Set up with [the development guide](docs/development.md), which covers the
toolchain, the build, the test suites and the WebUI workflow. The gates your
change must pass:

```bash
make fmt vet lint        # formatting, vet, the pinned linter
make test                # the suite
make test-race           # additionally, for anything streaming-adjacent
make check-tests         # refuses deleted, weakened or newly skipped tests
```

`make check-tests` compares against `origin/main`; it fails on a deleted
test, a weakened one, a new skip or a tautology, and reports without failing
the assertions that merely re-encode a production condition.

## Test doctrine

1. **Tests derive from specifications, not from code.** A test that only
   reproduces what the implementation does is a defect. Wire shapes come
   from the annotated fixtures under `testdata/golden/`, each carrying the
   URL it was read from and the date.
2. **RED-GREEN.** Write the failing test first, from the fixture or the
   reference, then make it pass in code. A test changes only when it is
   demonstrably misreading the spec — and the pull request says so.
3. **Streaming claims need streaming evidence.** Fragmentation, interleaving,
   pings, mid-stream failures and cancellation are covered by the harness,
   not by a mock that cannot fail the way an upstream does.

## Review checklist

Reviewers reject on:

- Dead methods or unused imports.
- A log call site that bypasses `internal/observability/redact` and could
  emit a credential.
- Post-mutation logging — logging state after it changed, instead of the
  transition.
- A race regression: removing a lock or a guard without replacing its
  guarantee.
- Snapshot tests of this proxy's own output used as a source of truth.
- A behaviour change that leaves the page describing it untouched.

No change is merged on its author's word alone; an independent review of
the wire facts is the norm here, not a formality.

WebUI changes additionally carry the console's own rules: tokens instead of
colour literals, block layout instead of wrapping variable-length text, and
screenshot baselines refreshed through the workflow rather than by hand
(all in the [development guide](docs/development.md#webui-development)).

## Toolchain

Use the current, safest versions — check the release *and* the dependents'
peer ranges before pinning, because "latest" is not "safest" when a plugin
cannot follow. The pinned versions, the deliberately held ones and the
upstream conditions that block them are in the
[development guide](docs/development.md#toolchain).

## Commits and pull requests

- Conventional commits (`feat:`, `fix:`, `docs:`, `test:`, `chore:`,
  `refactor:`), subject ≤ 72 characters, imperative mood, English.
- One logical change per commit; a change and its documentation in the same
  commit.
- A pull request says what changed, why, and how it was verified —
  including the commands you ran.
