# Protocol conformance matrix

The matrix proves every protocol pair — client surface → PROxyLLM → upstream
wire — with a deterministic, keyless, CPU-only setup. It is deliberately
separate from the unit suite: it drives the production codecs, transports and
HTTP surfaces against a real wire simulator.

## The two matrices

**M1 — protocol conformance (no CLI).** Official SDKs and raw HTTP drive a fully
assembled proxy whose providers point at
[MockServer](https://www.mock-server.com), an established open-source server
that encodes each provider's own wire format (OpenAI Chat and Responses,
Anthropic Messages, Gemini, Ollama NDJSON), including streaming framing, tool
calls, multi-turn sessions, and fault injection (429/529 with `Retry-After`,
mid-stream truncation, malformed SSE, quota).

**M2 — CLI matrix.** Every supported CLI is started the way a user starts it
(no special flags) in a pseudo-terminal, with only its own config file pointing
at the proxy, and run through the scenario set (single answer, 10+ turns, tool
loop, web search on turn 2+, cancel/resume, reconnect, large output, provider
faults, reasoning round-trip, vision fallback).

## Why no provider keys

MockServer is not a hand-written fake: it encodes the documented provider wire
byte-for-byte, and the official schemas plus the official SDK parsers validate
the result. A provider we hold no key for is still exercised end-to-end. Real
cloud checks are a separate, optional layer and would need one key per family;
they are not part of this matrix.

## Schema validation (per cell)

On top of the per-cell expectations, every pair cell schema-validates the exact
client-facing response bytes the proxy emitted — non-streaming documents against
the surface's document schema, streaming against the official per-frame schema
(chunks, Responses events, Gemini frames, Ollama NDJSON lines). The schemas live
in `internal/e2e/testdata/schemas/`, each with a `.meta.json` carrying the
official source URL, version/commit and access date — the same provenance rule
as the golden fixtures. Validation runs through
`github.com/santhosh-tekuri/jsonschema/v6` (JSON Schema 2020-12) and fails the
cell with named violations.

Cells whose surface cannot fully pass the *current* official schema carry a
`schema_note` in the manifest (`internal/e2e/testdata/matrix.json`): the gap is
named field-by-field with its official source, the note is printed on every run,
and the fix loop owns closing it (deleting the note re-enables full validation).
A cell is never silently exempt: the note is the contract.

## Running locally

```bash
# MockServer 9.2.0 (JAR or container), then:
java -jar mockserver.jar -serverPort 1080
MOCKSERVER_URL=http://127.0.0.1:1080 go test ./internal/e2e -run TestMatrix_ -v

# Or let the harness start a JAR you point at:
MOCKSERVER_JAR=/path/to/mockserver.jar go test ./internal/e2e -run TestMatrix_ -v
```

Without either variable the matrix tests skip with a clear reason; the
`matrix` workflow always runs them (MockServer is a service container).

## Status

Green today:

**The pair matrix (120 cells).** Every client surface × upstream wire ×
{text, tools} × {non-streaming, streaming}, driven by the two runners
`TestMatrix_TextPairs` and `TestMatrix_ToolPairs` — the manifest
(`internal/e2e/testdata/matrix.json`) is the single source of truth, the
runners only execute it.

**Scenario and fault cells.** Streaming round trips per surface, the
cross-protocol scenarios, the tool-call round trip, the thinking round trip
(enabled thinking reaches a thinking-capable upstream; the reasoning trace
surfaces beside the final answer), upstream 429/Retry-After, the 529
overload status (fail over, never a same-provider retry), mid-stream
truncation and malformed SSE (both must surface as terminal errors; the
malformed frame rides a raw Gemini SSE body because MockServer appends its
broken chunk after the terminal sentinel on sentinel-terminated wires),
MockServer's stateful quota window (the first request is served, the next one
over the limit is 429), the proxy's `insufficient_quota` cooldown (the first
call surfaces 402, the next is refused with 503 + Retry-After and never
reaches the upstream again), the emulated
web-tool flows (fully offline — MockServer also plays the Firecrawl v2
backend): the Anthropic search cell, the chat-surface client-web-tool claim,
the web_fetch cell (prior-context guard + document result), the plain-CLI
tool injection, count_tokens (local estimate, no upstream call), the
negative control (a 200 with a wrong wire shape must fail the client), the
`previous_response_id` chain on POST /v1/responses (the next upstream request
carries the restored first turn; an id the proxy does not hold is the
documented 404), the client-abort cell (a streaming client that cancels
mid-answer leaves a 499 log row, fires no re-dispatch, and the proxy answers
the next request normally — the MockServer simulation paces the stream with
`streamingPhysics` so the abort lands mid-answer deterministically), the
blind-chain vision fallback (a vision request rides a blind chain: the 400
modality rejection surfaces, the caption bootstrap describes the image, the
description replaces it and the same chain re-dispatches; the learned
`vision:false` lands in the blind model row), and the two decision surfaces
driven with the golden decision fixtures through their upstream providers
(`ollama-cloud` on the System One wire, `openai-decisions` on the Decisions
API), and the codex OAuth upstream (the account's seeded access token is
expired, so the first request forces the issuer rotation — MockServer also
plays the issuer's `/oauth/token` — and the freshly minted bearer plus the
id_token's account claim ride the ChatGPT backend request; the second request
reuses the minted token without another rotation).

**Gates.** `TestMatrix_ManifestIsComplete` enforces the full cross product, a
reason on every pending cell and the `schema_note` rule (a green pair cell is
either strict-validated or documents its gap precisely);
`.github/test-matrix/check_manifest.py` enforces that the manifest and the
Go tests agree in both directions, and the workflow runs the whole matrix
with `-count=2` for determinism.

**Meta-gates.** Three negative controls prove the suite can go red:
`TestMatrix_Meta_PairCellAssertionsHaveTeeth` (every pair assertion rejects a
wrong value), `TestMatrix_Meta_WireSchemaHasTeeth` (invalid JSON and an invalid
document fail the schema gate; the golden response passes), and
`TestMatrix_Meta_ManifestValidatorHasTeeth` (one broken manifest per rule).
`.github/test-matrix/test_gates.py` is the gate's own negative self-test,
including the duplicate-claim rule.

Pending cells (the manifest names the reason for each; the fix loop owns them):
- M2: scenarios beyond `answer`/`recall` are named skipped, with a reason, in
  `.github/test-matrix/cli_matrix.json`; the scenario implementation phases
  flip them green.

## M2 — the plain-start CLI matrix (cli-plain)

The `cli-plain` job in `.github/workflows/matrix.yml` drives
`.github/test-matrix/cli_plain.py`: 12 CLIs, each started exactly the way a
user starts it — the bare binary in a tmux pseudo-terminal, only its own
config pointing at the proxy — against MockServer.

**The manifest is the source of truth.** `.github/test-matrix/cli_matrix.json`
enumerates every client × scenario pair, status `green` or `skipped`; a skipped
cell carries its reason. `check_cli_manifest.py` fails on a missing pair, a
green scenario without a `SCENARIOS` function, a scenario function that is
green nowhere, a green cell outside a shard, or a shard the workflow does not
wire. `test_gates.py` is the gate's negative self-test. Today `answer`,
`recall`, `fault_429`, `fault_529`, `fault_quota` (all clients), the
`fault_truncated`/`fault_malformed_sse` pair (streaming clients), `web_turn2`
(all clients), `tool_loop` (crush, openhands), `multi_turn` (all clients) and
`large_output` (all but aider, whose TUI does not render the 20 KB tail) are
green across the `basic`, `faults`, `faults_stream`,
`web`, `tools` and `long` shards; the remaining scenarios are skipped with
their reason, so the omission is visible on every run instead of silent.

**What is skipped and why.** The manifest carries the exact reason on every
skipped cell; the recurring ones are: extended thinking (Claude Code's plain
TUI sends no `thinking` field even with a budget env — M1's Anthropic thinking
cell covers the proxy path), cancel/resume (no verified plain-start TUI
affordance — M1's client-abort cell covers the proxy), `previous_response_id`
(these clients send full history; `recall` plus the M1 Responses chain cover
the contract), vision (no verified plain-start image attach; the M1 blind-chain
cell covers the caption fallback), decisions (no CLI surface; M1 drives System
One and OpenAI Decisions), `tool_loop` on the CLIs whose tool seed or
approval flow is not verified yet, and `large_output` on aider (its TUI does
not render the 20 KB tail in the visible pane).

Each fault cell gets its own provider account (the breaker and quota cooldowns
are keyed by account, so sharing one would poison the next cell), and the CLI's
in-flight retries drain before the next cell resets MockServer. The quota cell
proves the CLI surfaces the 402 and then probes the same model directly: the
cooled account answers the probe with the surface's overload status (503 on
OpenAI, 529 on Anthropic) without reaching the upstream again. `fault_429`
accepts either recovery through a retry or an honest 429 row — what it never
accepts is a fabricated answer.

**Shards and evidence.** The workflow runs one job per manifest shard, each
with its own runner, home, port and `cli-plain-<shard>` artifact, and
`fail-fast: false`; a failing cell cannot hide another. Every cell writes
`result.json` next to its `pane.log` and `client-logs/` — status, duration,
pane tail, upstream request count, declared tool names, and the request-log
rows since the cell's baseline. A deliberate fault row from one cell can
therefore never poison a later cell's clean-row check.

**Why continue is not in the matrix.** cn 1.5.47's plain-start TUI never
mounts its composer on `ubuntu-24.04` runners: Ink paints exactly two stdout
frames, then the render loop emits nothing for the whole 45 s window while
the input effects keep running — no blocked syscall, no network wait — while
locally it mounts in every variant (node versions, locales, terminal sizes).
Scope: the plain-start TUI only; the headless M1 conformance cells are green
in CI throughout. Evidence chain: frame probe #114, strace #113, boot trace
#112/#111, deep dives #108–#115. The headless `live-cli` adapter stays (that
suite still runs it); the plain-start matrix does not carry a local-only
exception. Re-entry: add `continue` to `cli_matrix.json` when
@continuedev/cli (pinned at 1.5.47) mounts its composer on the runner image —
the pane artifacts prove any fix.

Out of scope (no client or product path uses them): embeddings, audio, images,
moderations, files, batches, Realtime/Live WebSocket, legacy completions,
Ollama `generate`/`embed`/`ps`/model management, Firecrawl map/crawl/extract/
batch/status, Anthropic admin/usage APIs, Ollama usage/balance endpoints.

## Rules

- No hand-written provider fake in this matrix; MockServer is the simulator.
- Every cell asserts the client-visible result **and** the upstream request
  shape, so a green cell proves both translation directions.
- A missing cell is a failure, never a silent skip (the completeness gate).
- Determinism: fixed ids/timestamps from the simulator; two runs assert the
  same.
- `TestPerfBudgets` is a pre-existing local failure on some machines and is
  unrelated to the matrix.
