# Claude Code with non-Claude models

Claude Code speaks the Anthropic Messages protocol, so it reaches PROxyLLM
through the Anthropic-compatible surface and can be routed to any model the
proxy serves — a hosted one, a local endpoint, a gateway. This page is the
client-side guide: how to point Claude Code at the proxy, how its model
names are mapped, and what a degraded run looks like.

## Pointing Claude Code at the proxy

```bash
export ANTHROPIC_BASE_URL="http://localhost:8080"
export ANTHROPIC_AUTH_TOKEN="pk_<prefix>_<secret>"   # a client key from the console
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

Claude Code performs its own fetch and search **client-side**: the model
asks for `WebFetch`, Claude Code retrieves the page itself and sends the
result in a follow-up request. The proxy sees only that follow-up — it never
sees the fetch, which is why a Claude Code WebFetch produces no
web-tool request in the console or in the log.

PROxyLLM's web-tool emulation serves a different thing: the **Anthropic
server-tool definitions** (`web_search_*`, `web_fetch_*`) that API clients
send. Those are intercepted, answered through the configured web-tool
accounts and returned as protocol-conform tool results, each one a
sub-operation row in the log.

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
| WebFetch returns nothing in the console's log | Working as designed — the fetch happens in Claude Code, see [above](#web-tools-webfetch-and-websearch) |
| Everything for one account fails at once | A cooldown: the Quota page names it, the trace's `skipped` events carry the reason |
