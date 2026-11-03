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
surfaces beside the final answer), upstream 429/Retry-After
and mid-stream truncation (must surface as errors), the emulated
web-tool flows (fully offline — MockServer also plays the Firecrawl v2
backend): the Anthropic search cell, the chat-surface client-web-tool claim,
the web_fetch cell (prior-context guard + document result), the plain-CLI
tool injection, count_tokens (local estimate, no upstream call), and
the negative control (a 200 with a wrong wire shape must fail the client).

**Gates.** `TestMatrix_ManifestIsComplete` enforces the full cross product, a
reason on every pending cell and the `schema_note` rule (a green pair cell is
either strict-validated or documents its gap precisely);
`.github/test-matrix/check_manifest.py` enforces that the manifest and the
Go tests agree in both directions, and the workflow runs the whole matrix
with `-count=2` for determinism.

Pending cells (the manifest names the reason for each; the fix loop owns them):
- The `previous_response_id` cell test (the fix itself is live from PR #79),
  vision fallback, client cancel, the codex OAuth upstream and
  the decision surfaces.
- M2: the 14 CLIs, plain start, full scenario set.

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
