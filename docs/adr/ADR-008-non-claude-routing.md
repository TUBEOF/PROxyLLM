# ADR-008: Non-Claude routing via the Anthropic surface — first-class feature

- **Status**: Accepted
- **Date**: 2026-09-10

## Context

Anthropic's own gateway documentation does not support routing Claude Code
to non-Claude models (fact C8, verified live 2026-09-10). Market practice
differs: gateways that translate the Anthropic `/v1/messages` surface to
OpenAI-compatible, Gemini and Bedrock upstreams document the combination as
a supported setup, and the community-proxy ecosystem treats it as the core
feature. The user decision
: this must work "nahezu reibungslos mit allen Models" —
near-seamless with all models — and be configurable in a dedicated WebUI
area.

## Decision

**First-class support**, governed by a per-model compatibility policy
(DB `models.compat` JSON, WebUI-editable):

| Setting | Values | Effect |
|---|---|---|
| `thinking_handling` | `strip` (default for non-Claude) / `map-to-reasoning-effort` / `block` | Strip removes thinking blocks + `thinking` param upstream — never fabricated signatures; map converts budget → `reasoning_effort` where the target supports it; block refuses with a protocol-conform error |
| `server_tool_emulation` | `native` / `emulate-firecrawl` / `off` | Firecrawl substitutes missing web tools (recorded in route_trace) |
| `max_tokens_clamp` | int / off | Clamp for lower-output targets (Claude Code assumes 32000 for unknown IDs) |
| `context_window` | int | Capability filtering + count_tokens heuristic |
| `cache_control` | `forward` / `drop` | Forward only on Anthropic→Anthropic; drop logs the degradation |

Global switch `anthropic_surface_non_claude = allow | warn | block`
(default `allow`). Every compat-policy application appears in route_trace —
never silent.

## Amendment (2026-10-11): unsolicited thinking blocks are dropped

A target's own reasoning was surfaced to the client as a `thinking` block
even when the request did not enable thinking — the reference returns
thinking blocks only for thinking-enabled requests, and a target that
reasons unprompted (Ollama Cloud's thinking models do, whether or not the
`think` parameter was sent) leaked its trace into a response shape the
reference never produces. Claude Code's helper calls send no thinking
parameter and read the FIRST content block as text, so the block read as an
empty answer: every `WebFetch` ended as "No response from model"
(reproduced against a deployed instance; the same shape without the fix on
a scratch instance).

Decision: `thinking_handling` now governs the response as well as the
request. The compat layer records on the request whether the CLIENT enabled
thinking (before the policy rewrites or drops the parameter — a stripped
parameter must not erase the client's intent), and the response path drops
thinking blocks — complete and streamed alike, renumbering streamed block
indexes so no hole reaches the client — when it did not. The drop is a
`compat_policy` `thinking_handling=strip-output` event, never silent.

Not changed: PROxyLLM still forwards no `think`/`reasoning_effort` for a
request that did not enable thinking, so a target that reasons anyway still
spends the tokens. Suppressing the output is what the client contract needs;
asking the target to stop is a separate, provider-specific change.

## Amendment (2026-10-14): the Claude family resolves, and a miss answers like the reference

Two coupled gaps broke WebFetch through Claude Code in the field, each
observed live against a scratch instance:

1. **Claude Code fires auxiliary requests under versioned Claude IDs its
   own release pins** — the WebFetch summarize call and the auto-mode
   classifier ran as `claude-haiku-5-5` (older releases: `claude-sonnet-*`)
   — and the alias map only matched names exactly, so an operator could not
   cover them without an entry per release. Family entries now resolve:
   an alias key that appears as a token of a `claude-*` name serves that
   name (`sonnet` covers `claude-sonnet-4-6`, `haiku` covers
   `claude-haiku-5-5`, and the dated spelling
   `claude-3-5-sonnet-20241022` alike). Exact entries win; the first
   matching token wins; the resolution is an `alias` trace event like any
   other — this is the Routing page's own promise, not a new default.

2. **The no-route 404 spoke prose, and Claude Code classifies model
   not-found by shape**: `404` + `not_found_error` + a message containing
   `model:`. Anything else is an opaque 404 — its fallback model never
   fires, and the fetch dies. The surfaces now answer the reference's
   spelling, `model: <id>` (credential shapes masked out of the name, which
   is client input and rides the log too). The router carries the model on
   a `*NoRouteError`; `errors.Is(err, ErrNoRoute)` is unchanged. Never a
   silent default model — the rejection still happens, it just says which
   model.

Verified live: family resolution end-to-end (alias event, live Ollama
Cloud answer), WebFetch through the real CLI, and the client fallback
firing on an unmapped name (`--fallback-model` retried after the conform
404). Pinned by `internal/smoke` smokes and unit tests on both surfaces,
stream and non-stream.

## Amendment (2026-10-16): alias patterns replace the family token rule

The 2026-10-14 amendment's family rule — an alias key matching a token of
a `claude-*` name — is removed. It was a second, implicit match mechanism
next to the exact lookup, and the overlap confused operators; aliases now
use one explicit language: a key is an exact name or a `*` wildcard, the
same pattern semantics the routing rules carry (`*` matches any run of
characters; a pattern without `*` is exact-only). An exact entry wins;
among matching patterns the one with the most literal characters does
(ties: lexicographic), so `claude-sonnet-*` beats `claude-*`
deterministically.

This is a breaking change for bare family entries: `sonnet` no longer
serves `claude-sonnet-4-6` (it remains an exact alias for a request
literally named `sonnet`); the migration is `sonnet` → `claude-sonnet-*`.
The reference-shaped no-route 404 and the `alias` trace event from that
amendment are unchanged, and the trace schema version does not move. The
log row and the detail drawer now render the hop (requested name →
configured target) from that event, so an alias is legible without
opening the trace.

## Consequences

- PROxyLLM deliberately goes beyond Anthropic's "not supported" stance,
  matching the documented practice of the translating gateways.
- Thinking-signature semantics are the one real gap: signatures cannot be
  fabricated for non-Claude targets, hence the explicit strip/map/block
  policy instead of silent corruption.
- A dedicated "Models & Compatibility" WebUI area holds every
  model's fallback chain, capabilities, and compat rules in one editor.