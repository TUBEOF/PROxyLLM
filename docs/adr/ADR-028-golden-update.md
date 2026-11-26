# ADR-028: Golden fixture update workflow (G1)

- **Status**: Accepted
- **Date**: 2026-11-26

## Context

The golden fixtures are the test oracle (ADR-006): a wire shape is transcribed
from the vendor's documentation into a source fixture with a `.meta.json`
that records the URL and the access date, and the derived expectations are
generated from it. When a vendor changes a shape, the source fixture is
re-read and re-transcribed by a human; the derived file must never be the
thing that is "fixed" to make a test pass.

Today an outdated `.expected.json` is edited by hand, which silently removes
the test's teeth. The work order asks for a local regeneration command with a
CI guard.

## Criteria

- P0: CI never regenerates an expectation. A diff is a failure.
- P0: only derived files can be regenerated; source fixtures cannot.
- P1: the regeneration is a review artifact with a visible diff.

## Options considered

| Option | Strengths | Weaknesses |
|---|---|---|
| Hand-edit expected files | No tooling | Removes the oracle; the known failure mode |
| Always regenerate in CI | No stale files | Tests stop checking anything |
| `GOLDEN_UPDATE=1` locally, CI refuses (this ADR) | Deliberate update; the diff is reviewed | One more mode in the test helper |

## Decision

`GOLDEN_UPDATE=1` makes the translate test utility regenerate **only**
derived `.expected.json` files under `internal/translate`. Source fixtures
(`*.json` transcribed from documentation) are never written by the tool.

Without the variable, a mismatch fails and prints the diff. A meta-test
refuses the update mode when `CI=true`, so the variable cannot be smuggled
into a pipeline. The update writes a normal git diff, which is the review
artifact — goldie/cupaloy semantics, no in-place trust.

`testdata/golden/README.md` documents the provenance rule and the exact
command; `docs/development.md` links to it.

## Consequences

- A small helper in `internal/translate/testutil`, one meta-test and
  documentation.
- A fixture refresh becomes: re-read the vendor documentation, update the
  source fixture and its `.meta.json`, run with `GOLDEN_UPDATE=1`, review the
  derived diff.
- `make check-tests` keeps guarding against weakened assertions; the update
  mode is not a way around it.
