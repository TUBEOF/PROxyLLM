# ADR-008: Non-Claude routing via the Anthropic surface — first-class feature

- **Status**: Accepted
- **Date**: 2026-09-10

## Context

Anthropic's own gateway documentation does not support routing Claude Code
to non-Claude models (fact C8, verified live 2026-09-10). Market practice
differs: LiteLLM ships a documented tutorial routing Claude Code to
OpenAI/Gemini/Bedrock via `/v1/messages` translation
(docs.litellm.ai/docs/tutorials/claude_non_anthropic_models), and the
community-proxy ecosystem (claude-code-router, UniClaudeProxy,
claude-code-ollama-proxy) treats it as the core feature. The user decision
(2026-09-10, OP-2): this must work "nahezu reibungslos mit allen Models" —
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

## Consequences

- PROxyLLM deliberately goes beyond Anthropic's "not supported" stance,
  matching LiteLLM's documented practice.
- Thinking-signature semantics are the one real gap: signatures cannot be
  fabricated for non-Claude targets, hence the explicit strip/map/block
  policy instead of silent corruption.
- A dedicated "Models & Compatibility" WebUI area (Phase 2) holds every
  model's fallback chain, capabilities, and compat rules in one editor.