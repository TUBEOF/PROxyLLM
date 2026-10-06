# Routing & Fallbacks

The routing engine (Analysis E) is deterministic and observable: every
skip, retry, failover, compat-policy application, tool-emulation
substitution, chain expansion, and strategy rotation lands in the request's
`route_trace` — **fallbacks are never silent**.

## Model identity (2026-09-21)

A model's routing identity is `<prefix>/<wire-name>` (e.g.
`ollamacloud/glm-5.3`): the prefix is a per-provider-type setting
(editable on the provider type's page, default = the type string), the
wire name is what the upstream expects. The models table stores both
(`ID` = routing identity, `Name` = wire name); the model sync creates
prefixed rows, and legacy unprefixed rows keep working (wire name = ID).
Before dispatch, the resolved chain's routing ID maps to the wire name
(row wins; a missing row falls back to the tail after the last slash) —
traced as `model_resolved` (route_trace v4). The client-facing response
echoes the routing identity, never the wire name. Aliases
(`model_aliases`) map client-facing names to routing identities and match
in the rule step below.

## Rule model (evaluation order)

1. **Key scope** — the client key's allowed model patterns.
2. **Model match** — exact model id, wildcard, or alias (the
   `model_aliases` setting maps Claude Code naming to your models).
   **Implicit routes (ADR-011):** when no enabled rule matches, the
   models table is consulted — a row for the requested model ID routes
   to that row's provider as a one-entry chain, traced as
   `implicit_route`. Rules always win (any matching rule suppresses the
   implicit route); key scopes and aliases resolve first as above. A
   dead, uncredentialed, or cooled-down provider still yields the honest
   `chain empty …` rejection with skip events — same behavior as an
   explicit rule. To take a model out of service without a rule, delete
   or disable its model row (or use key scopes).
3. **Chain resolution** — the ordered fallback chain for the matched
   rule. An entry names a provider ID, a `provider-id/model-id` pair, or
   a provider TYPE (ADR-009): a type entry expands to all accounts of
   that type at request time, so a newly added account joins the
   rotation without any rule edit. Accounts without a credential (an
   abandoned OAuth draft) stay out of the expansion.
4. **Strategy** (ADR-009) — how requests spread across the expanded
   chain. Per rule, with a global default (`routing_strategy` setting,
   default `round_robin`):
   - **Round Robin** — each request rotates the chain by one position;
     the rotation is a trace event.
   - **Fill-First** — the chain order is kept: the first entry serves
     everything until the health gate opens (breaker cooldown) or a 429
     exhausts its retry budget, then the next entry takes over.
5. **Capability filter** — deterministic surface/emulation capabilities
   derived from the request content (web_search, web_fetch,
   code_execution, streaming, context size vs. window); providers failing
   the filter are skipped **with a logged reason**. Vision is deliberately
   NOT gated here (2026-09-21 automatic capabilities): static provider
   flags cannot know which MODEL accepts images — the request goes out,
   and a real upstream rejection fails over.
6. **Health gate** — circuit breaker (consecutive failures), exponential
   backoff, probe recovery; cooled-down providers are skipped or
   deprioritized.
7. **Capability fallbacks** (ADR-010, amended 2026-09-21) — when the whole
   chain failed with `capability_gap` rejections (a 400 naming the input
   modality, e.g. "image input is not supported"), the global
   `vision_fallback` MODEL list (Routing page) gets one walk of its own in
   the executor — every consultation is a traced `capability_fallback`
   event. An empty list keeps the documented upstream error.

Config lives in the DB and is editable in the WebUI (**Routing**); rule
writes reach the live router without restart — only a successfully read
rule set swaps in (a failed store write keeps the last known-good rules).
The executor's attempt cap (2–3 providers per request) still applies to
the expanded chain — a very long account pool is not walked in one
request.

## Capability fallback lists (ADR-010, amended 2026-09-21)

The Routing page holds three global priority lists (drag-drop ordered,
saved in one write):

| List | Entries | Semantics |
|---|---|---|
| **Vision fallback** | MODEL IDs (routing identities, e.g. `ollamacloud/glm-5.3`) | After a chain exhausted on `capability_gap` rejections, the executor walks these models (each its provider's wire name); every use traced as `capability_fallback` |
| **Web search fallback** | Web-tool account IDs (firecrawl today) | The emulator pool tries accounts in order on emulated `web_search`; a failing account fails over to the next |
| **Web fetch fallback** | Web-tool account IDs | Same as above for `web_fetch` |

**Spec change 2026-09-21** (breaking for the vision list): entries are
MODEL IDs validated against the models table at save time — provider
IDs/types from v1.0.2 configurations no longer resolve and are skipped
with a log line until re-saved. The fallback moved from the router's
pre-stream gate to the executor: it fires only after the routed models
REALLY rejected the request at the upstream (HTTP 400 naming the input
modality — image, multimodal, unsupported content), never to rescue an
unresolvable chain (that stays the no-route error).

**Automatic capability learning**: every `capability_gap` rejection
**Wiring note (2026-10-06)**: the two web-tool lists are now CONSUMED —
until this fix the pool was assembled in registry account order and the
stored lists were validated but ignored. Pool assembly resolves each
entry's account to its emulator and walks the lists in priority order;
unknown accounts (e.g. a deleted account named in a stale list) are
skipped with a log line, and an empty list keeps the insertion order.

**Streaming emulation (2026-10-06)**: a STREAMING request whose tools
include emulated defs rides the complete-path emulation round trip to
finality, and the final response is synthesized into a protocol-conform
event stream. Latency trade: such a request has no token streaming (the
model's whole answer completes before the first byte) — the trade for
real tool results; relaying raw tool_use (the previous behavior) was
neither conform nor useful. The stream path's failover/capability
discipline is unchanged for requests WITHOUT emulated defs.

**Automatic capability learning**: every `capability_gap` rejection
persists the observed gap into the model row's capabilities (e.g.
`"vision": false`) — asynchronously, never on the request path. A pinned
flag (the operator's manual selection in the Models editor, written as
`"pinned": {"vision": true}`) stops the learning for exactly that
capability; the row's sync origin is preserved.

The web lists refine the executor's emulator pool: with exactly one
account the behavior is identical to the pre-ADR-010 single emulator; with
several, a dead backend fails over (the executor's emulation loop is
unchanged — the pool is just what it calls). Web-tool accounts are never
LLM-chain-routable; the rule editor refuses them.

## Failover semantics

- **Request-level retry** (connection errors, 429, 5xx — honoring
  `Retry-After`) only **before the first streamed byte**.
- Once streaming, an upstream failure terminates the stream with a
  protocol-conform error event — **no mid-stream provider swap**.
- The terminal chain error plus the full route trace is logged; the
  client sees the protocol error, the WebUI shows the transitions.

## Health tracking

A circuit breaker per provider opens after consecutive failures, backs
off exponentially, and probes for recovery. Cooled-down providers are
skipped with a route_trace reason; recovery is recorded too.

## Non-Claude routing & compatibility

On the Anthropic surface, non-Claude models are first-class: the global
`anthropic_surface_non_claude` switch (`allow`/`warn`/`block`) plus
per-model compat settings (`thinking_handling`, `server_tool_emulation`,
`max_tokens_clamp`, `context_window`, `cache_control`). See
[Claude Code with non-Claude models](claude-code-with-non-claude-models.md)
and the [translation matrix](reference/translation-matrix.md).

## Tool emulation

When a request needs `web_search`/`web_fetch` and the routed provider
lacks native support, the Firecrawl emulation layer substitutes
(protocol-conform result blocks) when the model's `server_tool_emulation`
is `emulate-firecrawl` — the substitution is recorded in route_trace and
the response metadata. Firecrawl accounts are connected in the WebUI
(Providers → Firecrawl, ADR-010); the `PROXYLLM_FIRECRAWL_API_KEY` ENV
remains the fallback when no account exists, and with neither, emulation
returns the documented "unavailable" error results. Multiple accounts +
the web fallback lists give the emulation real failover. Vision cannot be
emulated: a model that really rejects image input at the upstream fails
over within the chain, then to the configured vision fallback MODELS —
see above.

## Model sync (ADR-010)

Providers exposing model discovery (custom OpenAI-compatible `GET
/v1/models`, Ollama Cloud `GET /api/tags`, codex backend best-effort)
mirror their upstream list automatically: at startup and every
`model_sync_interval_hours` (default 24). Policy:

- New upstream models appear as **synced** rows (badged in the WebUI).
- Operator edits (capabilities, compat, priority) on existing rows
  survive every sync — only names refresh.
- Synced rows the upstream dropped are deleted; **manual rows are never
  touched** by the sync.
- Sync saves from the WebUI convert a row to manual (the sync stops
  tracking it).

Per-run outcomes (added/updated/removed/skipped per provider) land in the
`model_sync_state` setting and on the Models page; a dead upstream skips
loudly and never fails the run.