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
the axis: **`custom-openai` and `ollama-cloud` talk OpenAI Chat Completions**
(`ollama-cloud` at `https://ollama.com/v1`), **`codex-oauth` talks the
Responses wire**. No shipped upstream speaks the Anthropic or Ollama-native
wires (see Known degradations) — those codecs serve the client surfaces, so
an Anthropic→Anthropic route exists only as decode-encode against the IR.
Statuses: **native** (same-protocol, carried as-is), **translated** (codec
pair + IR), **degraded** (translated with a recorded loss), **emulated**
(routing supplies it against a target that lacks it), **not supported**
(refused or absent, documented here).

| Operation | → OpenAI Chat (custom-openai, ollama-cloud) | → Responses (codex) |
|---|---|---|
| Text content | translated | translated |
| Client tools: definitions, calls, results | translated | translated |
| Streaming | translated | translated (`stream: true` forced) |
| Extended thinking | translated — no wire field out; `reasoning` / `reasoning_content` decoded in | translated (`reasoning`, `encrypted_content`) |
| Images in user content | translated (`image_url` ↔ `images[]`) | translated (`input_image`) |
| Images in tool results | translated for OpenAI/Anthropic clients; **degraded when the target is the Ollama-native wire** (catalog №1) | translated |
| Images with an Anthropic file source | degraded — the `file_id` decodes away (catalog №2) | degraded (catalog №2) |
| Server tools (`web_search` / `web_fetch`) | emulated (web-tool accounts, ADR-015) | emulated |
| Stop reasons | translated | translated (`incomplete` reads as max-tokens) |
| Usage | translated (per-flavour counters, catalog №11) | translated |
| Cache markers | not supported (catalog №9) | not supported |
| Decisions (systemone / OpenAI Decisions client) | translated (ADR-017 pair, losses documented under Decision operations) | translated |

## Edge-case catalog

Numbered, each with the behaviour a request actually gets. Entries the codecs
already handle cite the handling; gaps carry **known gap** until fixed — the
fix lands with its fixture, matrix row, test and this entry's update.

1. **Images in tool results → Ollama-native target** — known gap: the
   `role: "tool"` encode carries text content only; `ImagePart`s are dropped
   silently (`internal/translate/ollama/ollama.go`, the RoleTool branch), so a
   sighted Ollama chain loses an image a tool returned without a trace event.
   Blind chains are unaffected — the caption substitution replaces tool-result
   images before encode (ADR-018).
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
8. **Interleaved thinking/content fragments (Ollama-native)** — buffered and
   emitted as one thinking block once content completes, so text never
   fragments.
9. **`cache_control`** — survives an Anthropic→Anthropic route only; every
   other crossing drops it by construction and records the loss.
10. **Guarded image URLs in captioning** — a remote image URL the caption
    path refuses to fetch leaves the image unplumbed: the unavailability
    placeholder stands in and the refusal is traced.
11. **Usage flavours** — `reasoning`-style and cached counters
    (`prompt_eval_cached_count`, Anthropic cache and web-request counters)
    normalize into the canonical usage; a flavour an upstream does not report
    stays absent, never zero-invented.
12. **Decision-image without a sniffable media type** — fails the encode with
    a clear error instead of a guessed data URL (see Decision operations).

## Request-side mapping

| Concept | OpenAI Chat Completions | OpenAI Responses | Anthropic Messages | Ollama native |
|---|---|---|---|---|
| System prompt | `system` / `developer` role | `instructions`, or a system-role input item | `system`; block form is preserved verbatim, a string re-encodes as one text block, non-text blocks are dropped | system-role message |
| Text content | `content` parts (a lone text part re-encodes as a plain string) | `input`, a string or typed items | `content` blocks (a string re-encodes as one block) | `content` string |
| Image | `image_url` part, URL or `data:` URI | `input_image` item | `image` block, URL or base64 source | `images[]`, bare base64 |
| Tool definitions | `tools[].function` | function item, flat — no nested `function` wrapper | `tools[]` | `tools[]`, function-shaped |
| Vision substitution (ADR-018) | On a chain whose every entry is known-blind, the executor replaces every image with placeholder TEXT (a `[Bild N (ref …)]` note) in the request and appends an `analyze_image` function definition to `tools`. The codec layers never see the tool: injection and answer interception happen against the canonical IR, so all four columns behave identically. The final response carries no `analyze_image` block on any surface |  |  |  |
| Tool call | `tool_calls[]`, deltas assembled by index | function-call item | `tool_use` block | `tool_calls[]`, object arguments |
| Tool call, one-shot arguments | The anchoring `tool_calls` delta may carry the complete `function.arguments` string — a spec-legal single-delta shape, emitted by `ollama.com/v1` (fixture `chat-stream-tool-oneshot.sse`). The decoder queues it for the newly opened block, and an encode toward an OpenAI client re-emits it in the anchor chunk instead of dropping it. | arguments are complete per entry | arguments are complete per entry | arguments are complete per entry |
| Tool definition without a schema | `parameters: {}` is always encoded: an absent schema, omitted by `omitempty`, can be rejected by a strict provider | `parameters` omitted | `input_schema` omitted | `parameters` omitted |
| Empty content (tool-call-only assistant turn) | `content: ""`, never `null` — Ollama's OpenAI-compatibility endpoint rejects null content with `400 invalid message content type: <nil>`, and an empty string decodes to zero parts | the message item is omitted | `content: []`, never null | `content: ""`, never null |
| `system` message inside `messages` | carried as a system-role message holding its text, never left as `content: null` (the same 400; agents append hook output and injected reminders mid-conversation) | system-role input item | kept verbatim | system-role message |
| Context-window marker `[1m]` / `[1M]` | stripped at routing, case-insensitively: the marker is never part of the model identity, so the base model routes, dispatches and matches — the strip is recorded as a `context_override` event ([route trace](route-trace.md)) | base model | base model | base model |
| Tool result | role `tool` message | `function_call_output` item | `tool_result` block | role `tool` message with text content |
| Extended thinking | `reasoning_effort` is read from the raw body into the request's thinking intent, which the compatibility policy then governs; the codec itself has no wire field for it and never forwards an effort hint | `reasoning` (effort) and `encrypted_content` | `thinking` parameter — enabled with a budget, or adaptive — plus thinking blocks carrying an opaque signature | `think`: an effort level (`high` / `medium` / `low` / `max`), or `true` for adaptive; a budget maps to the nearest level |
| Cache markers | — | — | `cache_control`, forwarded verbatim | — |
| Server tools | — | not represented — a tool item without a name is dropped | `server_tool_use` blocks, and dated `web_search_*` / `web_fetch_*` definitions whose stable identity is the family name | — |
| Request retention | — | `store: false` is always encoded: the proxy never asks an upstream to retain a request, and the ChatGPT/Codex backend rejects a body that leaves the field unset (codex-rs `ResponsesApiRequest.store`, fixture `openai-codex/responses-request.json`) | — | — |
| Request streaming | — | `stream: true` is required by the ChatGPT/Codex backend: a body that omits it is answered `400 {"detail":"Stream must be set to true"}` (observed live 2026-10-17; codex-rs always sets it, fixture `openai-codex/responses-request.json`). The codex provider encodes it unconditionally, and its non-streaming callers — routing's non-stream dispatch, image captioning, the Models-page test — are served by an internally streamed round trip assembled into one response | — | — |

## Response-side mapping

| Concept | OpenAI Chat | OpenAI Responses | Anthropic | Ollama native |
|---|---|---|---|---|
| Text | `choices[0].message.content` | `output_text` parts of a message item | `text` block | `content` |
| Tool call | `tool_calls[]`, with `finish_reason: tool_calls` | function-call item | `tool_use` block, with `stop_reason: tool_use` | `tool_calls[]` with object arguments |
| Thinking | `reasoning` / `reasoning_content` on the message, decoded into a thinking part; the chat encoder has no thinking channel | reasoning item | thinking block with its opaque signature | `message.thinking` |
| Stop reason | `finish_reason` | `status` (`incomplete` reads as a max-tokens stop) | `stop_reason` | `done_reason` |
| Usage | `prompt_tokens` / `completion_tokens` / `total_tokens` | usage on the terminal `response.completed` event | `input_tokens` / `output_tokens`, plus the cache counters and the `server_tool_use` web-request counters | `prompt_eval_count` / `eval_count`, plus `prompt_eval_cached_count` |

Thinking an upstream volunteers for a request that never enabled it is dropped
from the answer on every wire, streamed blocks included and renumbered so the
client never sees a gap: a client that asked for no thinking reads the first
content block as the answer. The policy behind it is in
[model compatibility](model-compat.md); the drop is recorded in the trace.

## Decision operations

Decision models are a second IR operation pair beside chat
([ADR-017](../adr/ADR-017-decision-models.md)):
typed questions scored against shared evidence in one non-streaming pass. Both
surfaces translate through `ir.DecisionRequest` / `ir.DecisionResponse`, so either
client shape reaches either upstream. Endpoint detail is on the
[decision API page](../api/decisions.md).

| IR (hub) | systemone (`POST /v1/systemone`) | OpenAI Decisions (`POST /v1/decisions`) |
|---|---|---|
| `Model` | `model` | `model` |
| `State` | `state` — a string; an object or array is carried as its serialized JSON text and re-emitted raw | `input` — a string; with images, one user message of `input_text` + `input_image` parts |
| `Images[]` | `images[]`, bare base64, request order | data URLs in `input_image.image_url` (`data:<media>;base64,…`); the media type is sniffed from the signature bytes when the source surface carried none |
| `Questions[].Name` / `.Type` / `.Instructions` | the key of the `questions` object; `choice` / `noul` / `score`; `instructions` | `questions[]` entries with `name` / `type`; `predicate` / `choice` / `score`; `instructions` |
| choice options, ordered | `criteria` object, key → description (`null` = bare label) | `choices[]` `{value, description}` |
| score levels, ordered (lowest first) | `criteria` string array | `levels[]` `{label, description}` |
| `KeepAlive` | `keep_alive`, raw passthrough | — dropped |
| `Answers[].Choice` + `Probabilities` + `Confidence` | `answers[name]` `{type, choice, probabilities{key: 0–1}, confidence}` | `answers[]` `{type, name, choice, probabilities[{value, probability}], confidence}` |
| `Answers[].Score` | `score` — the probability-weighted mean of the level indices; `legend` maps an index to its level text, `probabilities` is keyed by the same index | `score`; per-level `{value: <number>, label, probability}` entries |
| `Answers[].Probability` | `noul` — a number, not a Boolean | `predicate.probability` |
| `Usage` | `usage{input_tokens, output_tokens}`, declared required by the reference | none documented — parsed leniently when present, otherwise absent |
| refusal | — not representable | `answers[].type: "refusal"` |

**Documented losses** ([ADR-017](../adr/ADR-017-decision-models.md)):
a Decisions `level.description` has no systemone slot, which holds one string per
level — the label wins; a refusal answered by a Decisions upstream fails the
systemone surface with 502 instead of an invented answer; `keep_alive` is
systemone-only; a response whose upstream reports no usage omits the systemone
`usage` object rather than emitting zeros; an image without a recognizable
signature cannot become a data URL and fails the encode with a clear error.

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
- **Ollama native wire** (`/api/chat`) is still decoded and encoded for local
  servers, which attach through the `custom-openai` provider type, and is
  exercised by the codec contracts; no shipped provider uses it. Its limitation
  stands: interleaved thinking and content fragments decode as separate content
  blocks.
- **Cache markers** (`cache_control`) survive an Anthropic→Anthropic route only.
  No other wire shape has the field, so the marker is dropped by construction;
  an explicit `drop` policy also removes it and records the loss in the trace.
- **Token counting** is a local heuristic — characters ÷ 4 over the decoded
  text, calibrated against the observed ratio, images taking a flat allowance.
  It is never presented as an exact count: the Anthropic `count_tokens` surface
  answers the same estimate ([anthropic API](../api/anthropic.md#post-v1messagescount_tokens)),
  and the routing context-window gate compares it against a target's declared
  window.
