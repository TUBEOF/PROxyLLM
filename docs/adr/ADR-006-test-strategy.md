# ADR-006: Test strategy — spec-derived golden fixtures, RED-GREEN

- **Status**: Accepted
- **Date**: 2026-09-10

## Context

PROxyLLM's correctness lives in wire-protocol fidelity: two client
surfaces, multiple upstream protocols, streaming state machines, fallback
routing. The binding test doctrine, in the maintainer's words: "Tests,
Verifikation ist ein sehr wichtiger Punkt" — verification is the top
priority, including independent deep code reviews and visual verification
of the WebUI.

## Decision

**Doctrine (P0)**: tests derive exclusively from specifications (official
docs, schemas, examples) — never from generated code behavior. A test that
only reproduces code behavior is a defect.

1. **Golden fixtures from live docs**: every wire shape is captured as
   annotated JSON under `testdata/golden/` (source URL + access date per
   fixture); contract tests assert codecs against them.
2. **RED-GREEN**: tests first from fixture/spec, red, then green. Tests
   change only when demonstrably misreading spec (documented in PR).
3. **Streaming harness**: mock provider with scripted SSE (fragmented
   deltas, interleaved tool calls, pings, mid-stream errors, unknown event
   types) over real HTTP; full client→proxy→mock round trips incl.
   cancellation and slow-consumer backpressure.
4. **E2E acceptance gate**: official OpenAI SDK client AND
   Claude Code against the proxy; live smoke ENV-gated
   (`PROXYLLM_SMOKE=1`), never in default CI.
5. **Blocking CI gates**: gofmt/vet/golangci-lint, `go test -race`, tsc/
   eslint, CodeQL, gitleaks, govulncheck, npm audit, bundle budget.
6. **Review quality gates** (CONTRIBUTING): no dead methods, no unused
   imports, no post-mutation logging, no removed-lock race regressions.

**Verification & review doctrine (binding)**:

- **Research BEFORE building** — a non-trivial module starts with a live,
  cited research pass; the wire facts it establishes become golden fixtures
  with their source URL and access date.
- **Independent deep code reviews** — no code merged by its author alone;
  reviewer did not write the code. Charter: correctness vs spec, race/
  backpressure analysis on streaming paths, the security concept,
  test-quality audit, docs consistency. Blocking findings fixed and
  re-reviewed before merge.
- **Visual regression for the WebUI (mandatory)** — Playwright screenshot
  tests against the console's own rendered baselines, multi-viewport,
  empty/hover/focus states, design-rule assertions (tokens-only colors, no
  emoji, dark-only).
- **A verification report per change** — tests run + results, review
  findings + resolutions, visual diffs, budget numbers.

## Consequences

- Snapshot tests of proxy output as spec source are rejected (circular).
- Mocks generated from our own structs are rejected.
- CI is slower than "quick" — accepted; correctness of a proxy is protocol
  fidelity.