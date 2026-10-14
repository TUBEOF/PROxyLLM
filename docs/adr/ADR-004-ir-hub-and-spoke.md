# ADR-004: Translation engine — canonical IR hub-and-spoke

- **Status**: Accepted
- **Date**: 2026-09-10

## Context

PROxyLLM translates bidirectionally between the OpenAI protocol family
(chat/completions + responses) and the Anthropic Messages family, with
Ollama native as an additional wire shape. Pairwise converters would mean
N·(N−1) converters, each reimplementing streaming state machines.

## Decision

**Canonical IR hub-and-spoke**: each protocol implements two codecs + one
streaming state machine per direction; new providers add one codec pair.

`internal/translate/ir` defines: `Request` (model, system/instructions,
messages with typed parts — text, image, tool_call, tool_result, thinking,
usage hints, cache markers), `Response` (content blocks, stop reason,
usage), `StreamEvent` (start, block_start, delta variants, block_stop,
message_delta, done, error, ping).

Design rules:

- **Lossless-ish with explicit degradation**: non-representable wire fields
  ride in a per-request `passthrough` map, re-emitted on same protocol,
  dropped with a debug log (exact field path) only when crossing protocols.
  Documented per field in docs/reference/translation-matrix.md.
- **Streaming**: pushdown state machines per codec; SSE reader
  (bufio.Scanner), SSE writer flushing per event via `http.Flusher`;
  bounded channels for backpressure; cancellation via context from client
  disconnect to upstream.
- **Tool calls**: OpenAI index-based `tool_calls[]` deltas ↔ Anthropic
  sequential `tool_use` blocks; partial-JSON assembly state in the codec
  machine.
- **Thinking**: Anthropic thinking+signature ↔ OpenAI reasoning +
  encrypted_content on Responses-shaped targets; on incapable targets
  handled by per-model compat policy — never fabricated signatures.
- **Usage/caching**: cache_control passthrough on Anthropic→Anthropic only;
  cache token fields map into IR usage with explicit zeros.
- **Server tools**: versioned mapping table (config surface, not hardcoded
  constants) to capabilities; Firecrawl emulation produces protocol-conform
  blocks when the routed provider lacks native support.

## Consequences

- Adding a protocol = 2 codecs + 1 state machine, not 2(N−1) converters.
- The IR is a maintenance seam: as vendors rotate wire shapes (dated
  server-tool strings), mapping tables live in configuration.
- "OpenAI-as-IR" was rejected: it would demote Anthropic-specific semantics
  on the surface that must be lossless for Claude Code.