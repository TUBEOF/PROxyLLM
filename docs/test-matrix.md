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

Green today (existing surfaces):
- OpenAI Chat client → Ollama Cloud (OpenAI-compatible) upstream, streaming.
- OpenAI Responses client → Responses upstream.
- Anthropic Messages client → Anthropic upstream, streaming.
- Chat client → Anthropic upstream; Responses client → Anthropic upstream.
- Ollama native client → Ollama native upstream, NDJSON streaming.
- Chat client → Ollama native upstream.
- Chat client → Responses upstream, tool call.
- Faults: upstream 429/Retry-After and mid-stream truncation surface as errors.

Planned cells (red until the surface exists; the fix loop owns them):
- Native Gemini upstream (`generateContent`/`streamGenerateContent`) and the
  native Gemini client surface.
- Ollama Cloud's Anthropic-compatible `/v1/messages` upstream (only if a
  product path uses it) and the remaining kept pairings.
- Every client surface × every kept upstream wire, including the web-tool
  emulation flows.
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
