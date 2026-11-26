# ADR-025: M2 matrix reduction and a headless layer

- **Status**: Accepted
- **Date**: 2026-11-26

## Context

The M2 plain-start matrix drives twelve third-party CLIs. The nine adapters
outside the target set (aider, cline, crush, goose, gptme, interpreter, kimi,
openhands, qwen, plus continue) are the main source of CI flakes and
maintenance: each pins a version, installs its own runtime and breaks on
release. At the same time, several scenarios are unreliable in a TUI —
thinking, cancellation and resume cannot be triggered deterministically — so
those cells are skipped, and a skip that is not a real gap is a blind spot.

## Criteria

- P0: every supported CLI is exercised on the proxy path.
- P0: no silent skip — every exception is manifest-documented with a reason.
- P1: fewer pinned foreign runtimes, fewer flakes, fewer CI minutes.
- P1: the previously skipped scenarios become testable where the CLI allows it.

## Options considered

| Option | Strengths | Weaknesses |
|---|---|---|
| Keep twelve adapters | Broad coverage on paper | Flake source; most adapters are not supported products |
| Keep three, plain-start only | Small, stable | Thinking/cancel still skipped |
| Three adapters + headless layer (this ADR) | Stable set; TUI-hard scenarios move to a scriptable mode | Two modes per CLI; headless flags are version-sensitive |

## Decision

M2 supports exactly **claude-code, codex and opencode**. The other adapters,
their install steps and their manifest entries are removed; the removal is
recorded in `docs/test-matrix.md` so the history is not lost.

A headless layer is added on top of plain-start:

- `claude -p --output-format stream-json` for Claude Code,
- `codex exec` (JSON output) for Codex,
- `opencode run` for OpenCode.

Headless scenarios: `answer_headless`, `recall_headless`, `thinking_headless`,
`cancel_headless`. Plain-start stays for `answer`, `faults`, `web`, `tools`
and `long`, because it is the UX-realistic path.

Each adapter manifest pins the CLI version **and** the exact headless flags it
uses. A version bump without a cell re-run is not allowed. Shards become
basic / faults / web / tools / long; `faults_stream` folds into `faults` when
the cell count allows it.

Evidence per cell stays byte-based: `pane.log`, the client JSONL, the proxy DB
rows and the upstream request log. A green check without bytes is not a pass.

## Consequences

- Roughly 60 % fewer M2 CI minutes and a much smaller flake surface.
- Thinking and cancellation are covered on the proxy path instead of being
  skipped; `previous_response_id` and `vision` gain a realistic headless cell.
- The three structural gaps that no CLI exposes (for example malformed SSE)
  remain, each with a named manifest reason.
- `check_cli_manifest.py`, `test_gates.py`, `matrix.yml`, the adapter JSON and
  the matrix documentation change together; the manifest self-tests prove the
  gate still has teeth.
