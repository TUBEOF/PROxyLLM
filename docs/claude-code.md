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

Two limits stay honest. The claim applies on the Anthropic surface, the only
one whose wire can carry the family's result blocks. And the emulation's own
rules apply to the intercepted call: a `web_fetch` URL must have appeared in
the conversation (or in earlier search results), a definition's
`max_uses`/domain filters are enforced, and with no web-tool account configured
the call comes back as the documented `unavailable` error result instead of
silently doing nothing.

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
