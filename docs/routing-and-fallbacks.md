# Routing & Fallbacks

The routing engine (Analysis E) is deterministic and observable: every
skip, retry, failover, compat-policy application, tool-emulation
substitution, chain expansion, and strategy rotation lands in the request's
`route_trace` — **fallbacks are never silent**.

## Rule model (evaluation order)

1. **Key scope** — the client key's allowed model patterns.
2. **Model match** — exact model id, wildcard, or alias (the
   `model_aliases` setting maps Claude Code naming to your models).
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
5. **Capability filter** — capabilities derived from the request content
   (vision, web_search, web_fetch, code_execution, streaming, context
   size vs. window); providers failing the filter are skipped **with a
   logged reason**.
6. **Health gate** — circuit breaker (consecutive failures), exponential
   backoff, probe recovery; cooled-down providers are skipped or
   deprioritized.
7. **Capability fallbacks** (ADR-010) — when a request needs **vision**
   and the chain filtered to nothing, the global `vision_fallback` list
   (Routing page) is consulted: its entries resolve through the same
   expansion + capability/health gates as rule chains, and the
   consultation is a traced `capability_fallback` event. An empty list
   keeps the documented no-route error.

Config lives in the DB and is editable in the WebUI (**Routing**); rule
writes reach the live router without restart — only a successfully read
rule set swaps in (a failed store write keeps the last known-good rules).
The executor's attempt cap (2–3 providers per request) still applies to
the expanded chain — a very long account pool is not walked in one
request.

## Capability fallback lists (ADR-010)

The Routing page holds three global priority lists (drag-drop ordered,
saved in one write):

| List | Entries | Semantics |
|---|---|---|
| **Vision fallback** | Provider IDs or LLM provider types | Appended to a vision-less resolved chain; candidates pass the same capability/health gates; every use traced as `capability_fallback` |
| **Web search fallback** | Web-tool account IDs (firecrawl today) | The emulator pool tries accounts in order on emulated `web_search`; a failing account fails over to the next |
| **Web fetch fallback** | Web-tool account IDs | Same as above for `web_fetch` |

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
emulated: an all-chain vision gap is a protocol-conform capability-gap
error (or the vision fallback list routes to a capable provider first —
see above).

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