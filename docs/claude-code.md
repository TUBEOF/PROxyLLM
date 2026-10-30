# Claude Code with non-Claude models

Claude Code speaks the Anthropic Messages protocol, so it reaches PROxyLLM
through the Anthropic-compatible surface and can be routed to any model the
proxy serves — a hosted one, a local endpoint, a gateway. This page is the
client-side guide: how to point Claude Code at the proxy, how its model
names are mapped, and what a degraded run looks like.

## Pointing Claude Code at the proxy

```bash
export ANTHROPIC_BASE_URL="http://localhost:8080"
export ANTHROPIC_AUTH_TOKEN="pk_<prefix>_<secret>"   # an API key from the console
export ANTHROPIC_MODEL="claudecode/sonnet"           # any model row or alias
```

Claude Code authenticates with `Authorization: Bearer`; `x-api-key` is
accepted as well. `anthropic-version` and `anthropic-beta` are forwarded to
the upstream verbatim, so a beta feature it opts into is not dropped by the
proxy.

## Model names: aliases and wildcards

Claude Code asks for names like `claude-sonnet-4-6`. The **alias map** (in
the console's Routing page, stored as `model_aliases`) rewrites them before
any rule is matched:

```json
{
  "claude-sonnet-4-5": "gpt-5.2",
  "claude-sonnet-*": "ollamacloud/glm-5.3",
  "claude-haiku-*": "ollamacloud/gpt-oss:20b"
}
```

- A key is an **exact name** or a **`*` pattern** (`*` matches any run of
  characters).
- An exact entry always wins over a pattern. Among several matching
  patterns, the one with the most literal characters wins; ties resolve
  lexicographically, so there is exactly one deterministic answer.
- Resolution happens once: an alias target is never re-resolved.
- The whole map is live — a successful write takes effect without a restart.
  An invalid blob routes *without* aliases and logs, rather than failing
  startup.

**Patterns, not enumeration.** Claude Code fires auxiliary requests of its
own under versioned model IDs pinned by its release — the WebFetch
summarizer, the auto-mode classifier. Those IDs change as Claude Code
ships, so one alias per version is a losing race; a family pattern
(`claude-haiku-*`) is not. The dated legacy spelling
(`claude-3-5-sonnet-20241022`) does not match a modern family pattern and
needs its own entry if anything still asks for it.

**The bare family token is gone (2026-10-16).** A key without a `*` is
exact-only: `sonnet` no longer serves `claude-sonnet-4-6`. If you relied on
the old family-token behaviour, write `claude-sonnet-*` instead.

**An unmapped name answers `404`.** The body is the reference's
model-not-found envelope — `not_found_error` with the message
`model: <the name the client sent>`. That shape matters: Claude Code
treats a 404 as model-not-found (the precondition for its own model
fallback and re-selection) only when the type *and* the message match. A
prose message reads as an opaque 404, which is how a WebFetch ends up as an
empty answer or a bare "API error". Details in the
[error model](api/errors.md#model-not-found-404).

## The surface switch

`anthropic_surface_non_claude` governs the whole Anthropic surface:

| Value | Effect |
|---|---|
| `allow` (default) | Non-Claude models route normally |
| `warn` | The request proceeds; a `compat_policy` event is recorded in the trace |
| `block` | Non-Claude model names are refused with a clear 400; `claude-` targets are unaffected |

A missing or invalid value is always `allow` — the gate never fails closed.
It applies to `/v1/messages` only: `count_tokens` keeps answering estimates
for context management whatever the switch says.

## Compatibility

Each model carries its own policy — thinking handling, server-tool
emulation, output clamps, context window, cache markers. The settings and
what they do are documented once in
[model compatibility](reference/model-compat.md); two points matter most
when the target is not Claude:

- **Thinking signatures cannot be fabricated.** A non-Claude target has no
  way to produce a verifiable Anthropic signature, which is why the policy
  is explicit (`strip`, `map-to-reasoning-effort`, or `block`) instead of a
  silent approximation.
- **A target's own reasoning is dropped when you did not ask for thinking.**
  Claude Code's helper calls send no thinking parameter and read the first
  content block as the answer, so an unsolicited thinking block there would
  read as an empty reply. The trace records the drop.

## Web tools (WebFetch and WebSearch)

Claude Code ships its web tools as **ordinary client tools**: a raw capture of
its requests shows `WebFetch` and `WebSearch` among the function definitions,
with no dated server-tool `type` — and the CLI's own WebFetch would fetch the
page itself, so a proxy that only understood `web_fetch_*`/`web_search_*`
definitions would never see a web-tool request from it (no account walk, no
row in the log).

PROxyLLM therefore **claims those client tools** and serves them through the
web-tool accounts, exactly like a server-tool definition: the definition is
rewritten into the emulated family def (`web_fetch` / `web_search`, family
input schema, the client's own description), the model's call is intercepted
by the executor, answered by the web-tool accounts and returned as the
family's protocol-conform result blocks — each call a sub-operation row in the
log, named after the account that answered. The model never sees a difference:
it asks to fetch, the proxy fetches.

The mapping from the client's tool names to the families ships with the proxy
(`WebFetch` → `web_fetch`, `WebSearch` → `web_search`) and is overridable in
the `client_web_tools` settings KV — the names are the client's wire fact and
change with its releases. An empty object turns the claim off: the client then
runs its own tools as before, and the proxy neither fetches nor logs a row.

Two limits stay honest. The claim applies on every surface; only the Anthropic
wire carries the family's result blocks back, while a chat/Responses client
sees just the final answer. And the emulation's own rules apply to the
intercepted call: a `web_fetch` URL must have appeared in
the conversation (or in earlier search results), a definition's
`max_uses`/domain filters are enforced, and with no web-tool account configured
the call comes back as the documented `unavailable` error result instead of
silently doing nothing.

The proxy answers an emulated call even when the model's response also carries
calls the client must answer (a mixed turn), and the round-cap/budget exits
synthesize an `unavailable` result. A claimed family name is never handed back
as a client `tool_use`: Claude Code does not know `web_fetch`/`web_search` and
would report `No such tool available`, so a leaked lowercase name is a proxy
bug, not a client configuration issue. A forced `tool_choice` follows the
rename (and degrades to `auto` when `server_tool_emulation=off` removes the
definition).

Claude Code 2.1.x loads tool schemas on demand (`ToolSearch`). In some turns it
executes its own `WebSearch`/`WebFetch` client-side even when the proxy already
claimed the definition; that is not a proxy failure — the live test reports it
as `model_behavior`.

## Images (paste and screenshots)

Anthropic-native behavior first: a pasted image, or a Playwright MCP
screenshot, reaches the proxy as base64 `image` blocks in the conversation.
What happens next depends on whether the ROUTED model can see images —
nothing needs to change on the client side:

- **Sighted chain**: the blocks flow through natively; Claude Code cannot
  tell it is talking through a proxy.
- **Chain where every model row has learned (or has pinned) `vision: false`**:
  the image never reaches the dispatch. Each image becomes a
  `[Bild N (ref xxxxxxxxxxxx): <short description>]` placeholder and the
  executor injects an `analyze_image(image_ref, prompt)` tool for the model:
  the model sends the image plus its OWN question — "what is the exact error
  string?" — to the configured `vision_fallback` models and reads a targeted
  answer, as many times as the task needs. The tool is answered inside the
  request; Claude Code never sees the tool or any of its blocks, and its own
  tool calls are unaffected. One request of this class answers completely
  before the first streamed byte (see the emulated-tool streaming note in
  [routing and fallbacks](routing-and-fallbacks.md#streaming-requests-with-emulated-tools)).

The visibility of all of this is the log's: `image_caption` rows show what the
answering model was told, `analyze_image` rows name the vision model that
answered each ask, and the `capability_fallback` line records the
substitution itself. `vision_tool=0` returns the workflow to plain
descriptions only ([configuration](configuration.md#database-overrides-layer-4),
[ADR-018](adr/ADR-018-vision-passthrough-and-analyze-image.md)).

## What a degraded run looks like

Nothing is silent. The request's `route_trace` records every adaptation with
a `compat_policy` event and every substitution with the event of its kind —
the field names are in the [route trace reference](reference/route-trace.md).

The documented translation losses — no fabricated thinking signatures,
cache markers surviving only on an Anthropic→Anthropic route, heuristic
token counting, a target's reasoning dropped unless asked for — are listed
with the [translation matrix](reference/translation-matrix.md).

## Troubleshooting

| Symptom | Look at |
|---|---|
| Empty answers, or a bare "API error" | The log row's requested model: an unmapped auxiliary ID 404s. Add a family pattern to the alias map |
| Claude Code falls back to another model on its own | Expected: an unmapped name produced the model-not-found shape it recovers from |
| A run stops when the session grows | The target's context window versus the conversation; check the row's window and the trace's failure class |
| WebFetch returns nothing in the console's log | The claim is off — an empty `client_web_tools` mapping in the settings KV leaves the CLI's own tool to the CLI, see [above](#web-tools-webfetch-and-websearch) |
| Everything for one account fails at once | A cooldown: the Quota page names it, the trace's `skipped` events carry the reason |
