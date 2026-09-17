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

Config lives in the DB and is editable in the WebUI (**Routing**); rule
writes reach the live router without restart — only a successfully read
rule set swaps in (a failed store write keeps the last known-good rules).
The executor's attempt cap (2–3 providers per request) still applies to
the expanded chain — a very long account pool is not walked in one
request.

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
the response metadata. Without a Firecrawl key, emulation returns the
documented "unavailable" error results. Vision cannot be emulated: an
all-chain vision gap is a protocol-conform capability-gap error.