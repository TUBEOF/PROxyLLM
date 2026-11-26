# Translation matrix

Canonical field mapping between the protocol shapes PROxyLLM speaks, one table
per operation class. Every protocol contributes a codec pair against a shared
canonical IR, and crossing two protocols means two codecs plus the IR, so each
field is described once, from the IR outward
([ADR-004](../adr/ADR-004-ir-hub-and-spoke.md)).

**How to read a loss.** Nothing degrades silently. Every field marked below as
dropped, clamped or replaced is either recorded as an event in the request's
`route_trace` ([route trace](route-trace.md)) or named here — including the
crossings where a field has no equivalent in the target's shape. The
claims derive from the official documentation of each surface, the same sources
the golden fixtures cite, each carrying its source URL and access date
([golden fixtures](../../testdata/golden/README.md)).

Degradation that is decided per model — how a target that cannot express a field
is adapted — is the compatibility policy, documented once in
[model compatibility](model-compat.md). Endpoint behaviour belongs to the
[client API pages](../api/README.md).

**The rule for a new wire capability.** It lands with all four, in the same
change: a fixture under `testdata/golden/` citing the source and date, a row
in this matrix, a codec test derived from the fixture and the specification,
and — if any crossing is lossy — a named degradation below. A capability that
cannot carry all four is not added.

## Crossing matrix

Status of each operation class on the wires the shipped provider types
speak — every client surface funnels through the IR, so the target wire is
the axis. The shipped upstreams cover four wires: **`custom-openai` and
`ollama-cloud` talk OpenAI Chat Completions**, **`codex-oauth` talks the
Responses wire**, **`anthropic` talks the Messages wire**, and
**`ollama-native` talks `/api/chat`**. Client surfaces are Anthropic
Messages, OpenAI Chat, OpenAI Responses (HTTP and WebSocket) and Ollama
native.

Protocol directions:

| Client \ Provider | Chat | Responses | Anthropic | Ollama native |
|---|---|---|---|---|
| Anthropic Messages | translated | translated | native | translated |
| OpenAI Chat | native | translated | translated | translated |
| OpenAI Responses | translated | native | translated | translated |
| Ollama native | translated | translated | translated | native |

Statuses: **native** (same-protocol, carried as-is), **translated** (codec
pair + IR), **degraded** (translated with a recorded loss), **emulated**
(routing supplies it against a target that lacks it), **not supported**
(refused or absent, documented here).

| Operation | → OpenAI Chat (custom-openai, ollama-cloud) | → Responses (codex) | → Anthropic (`anthropic`) |
|---|---|---|---|
| Text content | translated | translated | translated |
| Client tools: definitions, calls, results | translated | translated | translated |
| Streaming | translated | translated (`stream: true` forced) | translated |
| Extended thinking | translated — no wire field out; `reasoning` / `reasoning_content` decoded in | translated (`reasoning`, `encrypted_content`) | translated (`thinking` blocks with signatures) |
| Images in user content | translated (`image_url` ↔ `images[]`) | translated (`input_image`) | translated (`image` block) |
| Images in tool results | translated for OpenAI/Anthropic clients; **degraded when the target is the Ollama-native wire** (catalog №1) | translated | translated |
| Images with an Anthropic file source | degraded — the `file_id` decodes away (catalog №2) | degraded (catalog №2) | degraded (catalog №2) |
| Server tools (`web_search` / `web_fetch`) | emulated (web-tool accounts, ADR-015) | emulated | translated (`server_tool_use` blocks) |
| Stop reasons | translated | translated (`incomplete` reads as max-tokens) | translated |
| Usage | translated (per-flavour counters, catalog №11) | translated | translated (cache counters, `server_tool_use` counters) |
| Cache markers | not supported (catalog №9) | not supported | forwarded verbatim |

## Edge-case catalog

Numbered, each with the behaviour a request actually gets. Entries the codecs
already handle cite the handling; gaps carry **known gap** until fixed — the
fix lands with its fixture, matrix row, test and this entry's update.

1. **Images in tool results → Ollama-native target** — supported: the
   `role: "tool"` encode carries text content and base64 image parts
   (`internal/translate/ollama/ollama.go`, the RoleTool branch). Blind chains
   still use the caption substitution before encode (ADR-018).
2. **Anthropic image blocks with `source.type: "file"`** — known gap: the IR
   image part has no file-id role and `wireImageSource` has no `file_id`
   field, so the id decodes away. Cross-protocol the image drops; Anthropic→
   Anthropic re-encodes an invalid source upstream.
3. **`data:` URI images from an OpenAI client** — decoded into a base64 part
   with its media type (`internal/translate/openai/datauri.go`); re-encoding
   rebuilds the URI, so the bytes survive transit.
4. **One-shot tool-call arguments** — a spec-legal single `tool_calls` delta
   carrying the complete `arguments` string (seen from `ollama.com/v1`,
   fixture `chat-stream-tool-oneshot.sse`): queued for the opening block and
   re-emitted in the anchor chunk toward OpenAI clients.
5. **Empty content on a tool-call-only turn** — `content: ""`, never `null`
   (Ollama's compatibility endpoint 400s on null); toward Responses the item is
   omitted, toward Anthropic `content: []`.
6. **`system` role inside `messages`** — carried as its own system-role
   message, never `content: null` (same 400 as №5).
7. **Unrequested thinking in a response** — dropped on every wire, streamed
   blocks renumbered so the client sees no gap; recorded in the trace.
8. **Interleaved thinking/content fragments (Ollama-native)** — decoded as
   separate content blocks at each kind change (BlockStop + BlockStart), not
   buffered; strict clients may render alternating text/thinking blocks. The
   OpenAI-compatible Ollama Cloud path buffers reasoning into one block.
9. **`cache_control`** — survives an Anthropic→Anthropic route only; every
   other crossing drops it by construction and records the loss.
10. **Guarded image URLs in captioning** — a remote image URL the caption
    path refuses to fetch leaves the image unplumbed: the unavailability
    placeholder stands in and the refusal is traced.
11. **Usage flavours** — `reasoning`-style and cached counters
    (`prompt_eval_cached_count`, Anthropic cache and web-request counters)
    normalize into the canonical usage; a flavour an upstream does not report
    stays absent, never zero-invented. Two surfaces are the documented
    exception — their official schemas (openai-openapi 73e5f9e; the
    anthropic-sdk-go v1.84 Usage required set — matrix fixtures, accessed
    2026-11-03) require the members even when the upstream reported nothing:
    the chat `chat.completion` document and its streaming usage chunk always
    carry the three token totals (explicit zeros), and the Anthropic Message
    carries the `cache_creation` TTL breakdown, `output_tokens_details` and
    `server_tool_use` objects (explicit zeros) plus serving metadata. The
    latter's strings are the reference's neutral values (tier `standard`,
    speed `standard`, region `global`) — never an invented count; the
    Responses usage detail objects (`cached_tokens` / `cache_write_tokens` /
    `reasoning_tokens`) ride explicit zeros the same way. Upstream-reported
    values are mapped through the IR and echoed verbatim.
12. **Text citations on a replayed assistant block** — the `citations` key
    carries two shapes: an array on a text block (a client replaying an
    earlier answer) and the config object `{"enabled":true}` on a document or
    server-tool definition. Both decode; the array is accepted and the text
    forwards without it (the IR has no citation field), while the config
    object still sets `ServerTool.CitationsEnabled`. Pinning the object shape
    used to fail the whole request with a 500.
13. **Tool calls without an id** — native Ollama's `tool_calls` carry no id.
    The official client-facing objects all require one, so the emission mints
    `call_<n>` (OpenAI chat document, Responses `call_id`) or `toolu_<n>`
    (Anthropic `tool_use`) per response, deterministically; the client replays
    the minted id as `tool_call_id` / `tool_use_id` and the loop stays closed.
    The canonical Anthropic `content_block_start` stream block already carried
    an explicit `"id"` member, so streaming needs no mint.

## Request-side mapping

| Concept | OpenAI Chat Completions | OpenAI Responses | Anthropic Messages |
|---|---|---|---|
| System prompt | `system` / `developer` role | `instructions`, or a system-role input item | `system`; block form is preserved verbatim, a string re-encodes as one text block, non-text blocks are dropped |
| Text content | `content` parts (a lone text part re-encodes as a plain string) | `input`, a string or typed items | `content` blocks (a string re-encodes as one block) |
| Image | `image_url` part, URL or `data:` URI | `input_image` item | `image` block, URL or base64 source |

A `file` source on an Anthropic image block (a Files-API `file_id`) rides the
IR as an identity: an Anthropic→Anthropic round trip hands the id back, and a
crossing protocol **drops the image** — the id names a file only the origin
account can fetch, so no other wire can carry it (fixture
`anthropic/message-request-file-image.json`). The caption path refuses such an
image like a guarded URL: no fetchable bytes means no description — the
unavailability placeholder stands in and the refusal is traced, never a
fabricated caption shared by every file id.
| Tool definitions | `tools[].function` | function item, flat — no nested `function` wrapper | `tools[]` |
| Vision substitution (ADR-018) | On a chain whose every entry is known-blind, the executor replaces every image with placeholder TEXT (a `[Bild N (ref …)]` note) in the request and appends an `analyze_image` function definition to `tools`. The codec layers never see the tool: injection and answer interception happen against the canonical IR, so all three target columns behave identically. The final response carries no `analyze_image` block on any surface |  |  |
| Tool call | `tool_calls[]`, deltas assembled by index | function-call item | `tool_use` block |
| Tool call, one-shot arguments | The anchoring `tool_calls` delta may carry the complete `function.arguments` string — a spec-legal single-delta shape, emitted by `ollama.com/v1` (fixture `chat-stream-tool-oneshot.sse`). The decoder queues it for the newly opened block, and an encode toward an OpenAI client re-emits it in the anchor chunk instead of dropping it. | arguments are complete per entry | arguments are complete per entry |
| Tool definition without a schema | `parameters: {}` is always encoded: an absent schema, omitted by `omitempty`, can be rejected by a strict provider | `parameters` omitted | `input_schema` omitted |
| Empty content (tool-call-only assistant turn) | `content: ""`, never `null` — Ollama's OpenAI-compatibility endpoint rejects null content with `400 invalid message content type: <nil>`, and an empty string decodes to zero parts | the message item is omitted | `content: []`, never null |
| `system` message inside `messages` | carried as a system-role message holding its text, never left as `content: null` (the same 400; agents append hook output and injected reminders mid-conversation) | system-role input item | kept verbatim |
| Context-window marker `[1m]` / `[1M]` | stripped at routing, case-insensitively: the marker is never part of the model identity, so the base model routes, dispatches and matches — the strip is recorded as a `context_override` event ([route trace](route-trace.md)) | base model | base model |
| Tool result | role `tool` message | `function_call_output` item | `tool_result` block |
| Extended thinking | `reasoning_effort` is read from the raw body into the request's thinking intent, which the compatibility policy then governs; the codec itself has no wire field for it and never forwards an effort hint | `reasoning` (effort) and `encrypted_content` | `thinking` parameter — enabled with a budget, or adaptive — plus thinking blocks carrying an opaque signature |
| Cache markers | — | — | `cache_control`, forwarded verbatim |
| Server tools | — | web search: the Responses `web_search_call` item (`id`, `status`, `action{type:"search",query}`; stream: added→done→completed snapshot; results ride annotations, never the item) | `server_tool_use` blocks, and dated `web_search_*` / `web_fetch_*` definitions whose stable identity is the family name |
| Request retention | — | `store: false` is always encoded: the proxy never asks an upstream to retain a request, and the ChatGPT/Codex backend rejects a body that leaves the field unset (codex-rs `ResponsesApiRequest.store`, fixture `openai-codex/responses-request.json`) | — |
| Request streaming | — | `stream: true` is required by the ChatGPT/Codex backend: a body that omits it is answered `400 {"detail":"Stream must be set to true"}` (observed live 2026-10-17; codex-rs always sets it, fixture `openai-codex/responses-request.json`). The codex provider encodes it unconditionally, and its non-streaming callers — routing's non-stream dispatch, image captioning, the Models-page test — are served by an internally streamed round trip assembled into one response | — |

An Anthropic user message may carry `tool_result` blocks beside text (Claude
Code appends reminder text in the same turn). A crossing encode splits it: one
role-`tool` message per result carrying its `tool_call_id` (the chat wire
requires each `tool_calls` entry to be answered by a `tool` message), then the
remaining user content as its own message.

## Response-side mapping
| Concept | OpenAI Chat | OpenAI Responses | Anthropic |
|---|---|---|---|
| Text | `choices[0].message.content` | `output_text` parts of a message item | `text` block |
| Tool call | `tool_calls[]`, with `finish_reason: tool_calls` | function-call item | `tool_use` block, with `stop_reason: tool_use` |
| Thinking | `reasoning` / `reasoning_content` on the message, decoded into a thinking part; the chat encoder has no thinking channel | reasoning item | thinking block with its opaque signature |
| Stop reason | `finish_reason` | `status` (`incomplete` reads as a max-tokens stop) | `stop_reason` |
| Usage | `prompt_tokens` / `completion_tokens` / `total_tokens` — always emitted (zeros explicit, like the streaming usage chunk; catalog №11) | usage on the terminal `response.completed` event, detail objects always present (`cached_tokens`+`cache_write_tokens`, `reasoning_tokens`) | `input_tokens` / `output_tokens`, plus the cache counters and the `server_tool_use` web-request counters, the `cache_creation` TTL breakdown, `output_tokens_details` and the serving metadata strings (catalog №11) |

Thinking an upstream volunteers for a request that never enabled it is dropped
from the answer on every wire, streamed blocks included and renumbered so the
client never sees a gap: a client that asked for no thinking reads the first
content block as the answer. The policy behind it is in
[model compatibility](model-compat.md); the drop is recorded in the trace.

**Response envelopes are the official schemas, not a subset.** The three
client-facing chat-family surfaces validate their emitted bytes against the
official wire schemas in the conformance matrix (`internal/e2e/testdata/
schemas/`, provenance in each `.meta.json`, accessed 2026-11-03), so the
emission carries every member those schemas require — including the ones the
IR does not track, rendered with the references' own neutral values:

| Surface (ir.Response → wire) | Always-present members | Neutral value when untracked |
|---|---|---|
| OpenAI Chat `chat.completion` | `created`, per-choice `logprobs`, usage totals | `created` minted at encode; `logprobs: null`; usage zeros |
| OpenAI Responses `response` (document and stream snapshots) | `access_programs`, `created_at`, `error`, `incomplete_details`, `instructions`, `tools`, `parallel_tool_calls`, `metadata`, `tool_choice`, `temperature`, `top_p`; item ids; output_text `annotations`/`logprobs`; usage detail objects | null / minted at encode / `[]` / `"auto"` / `1.0` / `true` / `{}` / `[]` — exactly the streaming snapshots' values |
| Anthropic Message (document and `message_start`) | `container`, `stop_details`, `diagnostics`, `stop_sequence`, the usage sub-objects and serving strings | explicit nulls; `stop_sequence` rides the IR; serving `standard`/`standard`/`global` (reference defaults) with upstream-reported values echoed through the IR |

## Known degradations

- **Ollama Cloud** rides the OpenAI-compatible `/v1/chat/completions` surface
  (`https://ollama.com/v1`, docs accessed 2026-09-22), not the native
  `/api/chat` wire: the native wire interleaves thinking and content fragments,
  which fragmented the proxy's text output. Reasoning arrives as
  `delta.reasoning` (Ollama) or `delta.reasoning_content` (DeepSeek-style
  providers) on the stream, and as `message.reasoning_content`
  non-streaming — an asymmetry documented upstream. The proxy buffers
  interleaved reasoning fragments and emits one thinking block once the content
  is complete, so the text never fragments. Model IDs come from `GET /v1/models`
  (OpenAI list shape) and may differ from the native `/api/tags` names. Server
  tools cannot ride to this target and fall to web-tool emulation like any other
  OpenAI-compatible target.
- **Ollama native wire** (`/api/chat`) is a shipped upstream (`ollama-native`)
  and a client surface (`POST /api/chat`, `GET /api/tags`). Its limitation
  stands: interleaved thinking and content fragments decode as separate
  content blocks at each kind change.
- **Cache markers** (`cache_control`) survive an Anthropic→Anthropic route only.
  No other wire shape has the field, so the marker is dropped by construction;
  an explicit `drop` policy also removes it and records the loss in the trace.
- **Token counting** is a local heuristic — characters ÷ 4 over the decoded
  text, calibrated against the observed ratio, images taking a flat allowance.
  It is never presented as an exact count: the Anthropic `count_tokens` surface
  answers the same estimate ([anthropic API](../api/anthropic.md#post-v1messagescount_tokens)),
  and the routing context-window gate compares it against a target's declared
  window.
