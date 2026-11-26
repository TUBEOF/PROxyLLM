# ADR-027: CI extensions — ShellCheck, OpenAPI contract, spec sync

- **Status**: Accepted
- **Date**: 2026-11-26

## Context

Three classes of defect currently reach `main` because no gate sees them:

- shell scripts (the installer, the test harness) are unlinted;
- the admin API surface (`docs/api/openapi.json`) can lose or change an
  operation without a failing check, even though the embedded console is its
  first consumer;
- the OpenAI surfaces are validated against hand-maintained schemas only, not
  against the official OpenAPI document.

The work order also adds a third way to run the MockServer-based matrix
(testcontainers) for contributors, keeping the existing skip path when Docker
is absent.

## Criteria

- P0: a missing operation or a removed path in `openapi.json` fails CI.
- P0: a shell script with a ShellCheck finding fails CI.
- P1: spec drift is caught against a pinned upstream document, with
  provenance.
- P1: no new hard dependency where an existing mechanism suffices.

## Options considered

| Option | Strengths | Weaknesses |
|---|---|---|
| Lint/contract checks only on release | Fewer PR minutes | Defects land on `main` and block the release |
| All three gates on every PR (this ADR) | Fail fast; the console's contract stays honest | A few minutes per PR; one new pinned action and one pinned document |
| Replace hand-written schemas with the official spec | One source | The official document is much larger than the cells need; a pinned extract is the compromise |

## Decision

1. **ShellCheck gate** in the lint job for `scripts/**`, with the action
   version pinned like every other action. Zero findings is the bar.
2. **OpenAPI contract gate**: a CI step diffs `docs/api/openapi.json` against
   `origin/main`. Removing an operation id or a path fails; a changed schema
   is reported in the job summary. The gate is self-tested (remove a path,
   expect red, restore).
3. **Spec-sync cells** in M1: responses from the OpenAI surfaces are validated
   against a pinned copy of the official `openai/openai-openapi` document
   (only the chat and responses parts, with `.meta.json` source, commit and
   date), and the Ollama surface against a pinned derivation of its OpenAPI.
   `kin-openapi` is evaluated against a `jsonschema`-derived extract; the
   decision and its cost are recorded in the work-package report. A local
   schema that disagrees with the pinned official document is a bug.
4. **testcontainers-go** module for the MockServer as a third local path. The
   existing environment-variable/JAR path and its skip behaviour are
   unchanged; Docker being absent keeps the existing skip.

## Consequences

- `ci.yml` gains two steps, `matrix.yml` gains cells, `Makefile` gains targets
  and `docs/development.md` explains all of them.
- A new Go dependency is possible for the spec-sync cell; the `go.mod` diff is
  kept minimal and justified in the PR.
- The contract gate protects the admin API consumers (the console first) from
  accidental removals; intentional removals land with the `openapi.json`
  change in the same PR.
- ShellCheck findings are fixed rather than suppressed; a justified
  suppression carries an inline reason and appears in the PR.
