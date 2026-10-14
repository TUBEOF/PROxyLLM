# Translation Matrix (Reference)

Canonical field mapping between the four protocol shapes PROxyLLM speaks.
This is the reference for the canonical-IR hub-and-spoke translation engine
(ADR-004): each protocol contributes a codec pair; crossing protocols means two
codecs + the IR.

Legend for non-mappable fields: fields that exist on one protocol only
ride in the per-request **passthrough map** — re-emitted verbatim when the
response crosses the same protocol, dropped with a debug log (exact field
path) when crossing protocols. Degradation is documented per field below
and recorded in `route_trace` where a compat policy applies it; policy
application is never silent.

## Request-side mapping

| Concept | OpenAI Chat Completions | OpenAI Responses | Anthropic Messages | Ollama native |
|---|---|---|---|---|
| System prompt | `system` / `developer` role | `instructions` / system items | `system` (block form preserved verbatim) | system role |
| Text content | `content` parts | `input` string or typed items | `content` blocks | message content |
| Image | `image_url` part (URL or data: URI) | `input_image` item | `image` block (URL or base64) | base64 images |
| Tool definitions | `tools[].function` | function items | `tools[]` | `tools` |
| Tool call | `tool_calls[]` (index-assembled deltas) | function-call items | `tool_use` blocks | `tool_calls` |
| Tool call, one-shot arguments | the anchoring `tool_calls` delta may carry the COMPLETE `function.arguments` string (spec-legal single-delta shape; ollama.com/v1 emits this) — the decoder queues it for the newly opened block, never drops it (fixture `chat-stream-tool-oneshot.sse`); anchors carrying IR `Input` re-emit it in the anchor chunk toward OpenAI clients | same | same | object arguments (always complete per entry) |
| Tool definition without `input_schema` | `parameters: {}` is always encoded (an absent schema is dropped by `omitempty` upstream and can be rejected by strict providers) | schema-less function item | `input_schema: {}` (fixture shape) | `parameters: {}` |
| Empty content (tool-call-only assistant turn) | `content: ""` — never `null` (Ollama's OpenAI-compat endpoint rejects null with 400 `invalid message content type: <nil>`; both decode to zero parts) | field omitted | blocks (never null) | string content (never null) |
| role `system` message inside `messages` | encoded as a system-role message carrying its text — never falls through the role switch leaving `content: null` (same Ollama 400; Claude Code appends one for hook output / injected reminders) | system-role input item | kept verbatim | system role |
| Context-window marker `[1m]`/`[1M]` on the model name (Claude Code's 1M override, e.g. `glm-5.3-flash[1M]`) | stripped at routing (`context_override` trace event, route_trace schema v6) — the base model routes and dispatches; the marker is never part of the model identity, so operators express matchers with the base name | input item carries the base model | base model | base model |
| Tool result | role `tool` message | `function_call_output` item | `tool_result` block | tool response role |
| Extended thinking | — | `reasoning` + `encrypted_content` | `thinking` param + thinking blocks (signature opaque); blocks the request never enabled are dropped from the answer | `reasoning_effort` |
| Cache markers | n/a | n/a | `cache_control` (forwarded verbatim on Anthropic→Anthropic only) | n/a |
| Server tools | n/a | `web_search` etc. | server_tool_use blocks | n/a |

Per-model compatibility policy (`models.compat`) governs degradation on
non-Claude targets — see the compat table below.

## Response-side mapping

| Concept | OpenAI Chat | OpenAI Responses | Anthropic | Ollama native |
|---|---|---|---|---|
| Text | `choices[0].message.content` | `output_text` / message item | `text` block | content |
| Tool call | `tool_calls[]` with `finish_reason: tool_calls` | function-call item | `tool_use` block + `stop_reason: tool_use` | `tool_calls` |
| Thinking | — | reasoning items | thinking blocks (+ signature) | stripped (compat policy) |
| Stop reason | `finish_reason` | `status`/finish | `stop_reason` | `done_reason` |
| Usage | `prompt_tokens` / `completion_tokens` / `total_tokens` | usage in terminal events | `input_tokens` / `output_tokens` (+ cache fields) | eval counts |

## Compat policy effects (non-Claude targets)

Per-model compatibility settings (`models.compat` in the DB, WebUI
"Models & Compatibility" editor) govern the translation on the
Anthropic surface:

| Setting | Values | Effect on translation |
|---|---|---|
| `thinking_handling` | `strip` (default for non-Claude) / `map-to-reasoning-effort` / `block` | strip removes thinking blocks and the `thinking` param upstream — never fabricates signatures back; map converts `budget_tokens` → `reasoning_effort` where the target supports it; block refuses with a protocol-conform error. Independent of the setting, an upstream's OWN reasoning is dropped from the answer when the request did not enable thinking (`strip-output` event): the reference returns thinking blocks only for thinking-enabled requests, and a client that asked for none reads the first content block as text |
| `server_tool_emulation` | `native` / `emulate-firecrawl` / `off` | Whether Firecrawl substitutes missing web tools; substitution is recorded in route_trace |
| `max_tokens_clamp` | int / off | Clamp for targets with lower output limits (C7 default 32000 for unknown model IDs) |
| `context_window` | int | Context window for capability filtering + the count_tokens heuristic |
| `cache_control` | `forward` / `drop` | `forward` on Anthropic→Anthropic only; `drop` logs the degradation |

## Known degradations (v1)

- **Ollama Cloud** (since 2026-09-22): rides the OpenAI-compatible
  `/v1/chat/completions` surface (`https://ollama.com/v1`, docs
  accessed 2026-09-22), not the native `/api/chat` wire — the native wire
  interleaves thinking and content fragments, which fragmented the proxy's
  text output (the Claude Code rendering bug). Reasoning rides
  `delta.reasoning` (Ollama) / `delta.reasoning_content` (DeepSeek-style)
  on the stream and `message.reasoning_content` non-streaming (the field
  asymmetry is documented upstream, ollama/ollama#17911); the proxy
  buffers interleaved reasoning fragments and emits ONE thinking block
  after the content — the text never fragments. Model IDs come from
  `GET /v1/models` (OpenAI list shape) and may differ from the native
  `/api/tags` names. Server tools fall to Firecrawl emulation (§6.2) as
  for any other OpenAI-compat target.
- **Ollama native wire** (`/api/chat`): still decoded/encoded for local
  servers via custom-openai or the codec tests, but no provider ships on
  it since 2026-09-22. Its known limitation stands: interleaved
  thinking/content fragments decode as separate content blocks.
- **Cache markers** (`cache_control`) only survive Anthropic→Anthropic;
  every other crossing drops them with a debug log naming the field path.
- **Token counting** on non-Anthropic-shaped upstream is heuristic
  (chars/4 calibrated), marked `estimation: heuristic` (C1).
- **count_tokens** on the Anthropic surface: native passthrough for
  Anthropic-shaped upstream, heuristic otherwise.

## Source basis

Mapping derived from the official protocol documentation of each surface — the
same sources the golden fixtures in `testdata/golden/` cite, each with the URL and
the date it was read.