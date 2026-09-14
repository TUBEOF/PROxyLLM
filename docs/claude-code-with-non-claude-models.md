# Claude Code with Non-Claude Models

Claude Code talks to PROxyLLM via the Anthropic-compatible surface
(`POST /v1/messages`). Routing Claude Code to non-Claude models (OpenAI-
compatible providers, Ollama Cloud, custom providers) is a **first-class
feature** (plan OP-2), not a workaround. This guide documents the setup,
the compatibility policy, and how degradation is made visible.

## Pointing Claude Code at the proxy

```bash
export ANTHROPIC_BASE_URL="http://localhost:8080"
export ANTHROPIC_AUTH_TOKEN="pk_<prefix>_<secret>"   # client key from the WebUI "Keys" area
export ANTHROPIC_MODEL="my-openai-model"             # or configure aliases (below)
```

Claude Code sends `POST /v1/messages` with `Authorization: Bearer` (and/or
`x-api-key`); PROxyLLM accepts both header families. `anthropic-version`
and `anthropic-beta` are forwarded verbatim.

## Model aliases (Claude Code naming → your models)

Claude Code requests model IDs like `claude-sonnet-4-*`. The WebUI
"Routing" area maps these to your configured models via the
`model_aliases` setting (JSON object alias → model-id). A successful
write takes effect without restart; an invalid blob routes without
aliases (logged) until fixed.

```json
{
  "claude-sonnet-4-5": "gpt-5.2",
  "claude-haiku-4-5": "ollamacloud/qwen3:32b"
}
```

## The global surface switch

The `anthropic_surface_non_claude` setting (WebUI "Routing" area, DB
override layer) governs the whole Anthropic surface:

| Value | Effect |
|---|---|
| `allow` (default) | Non-Claude models route normally |
| `warn` | Non-Claude requests proceed; a `compat_policy` event is recorded in `route_trace` (WebUI log detail shows the banner) |
| `block` | Non-Claude model names are refused with a clear 400 error; Claude targets (`claude-` prefix) are never affected |

A missing or invalid value is always `allow` — the gate never fails
closed. The gate applies to `/v1/messages` only; `/v1/messages/count_tokens`
is deliberately not gated (it must keep answering token estimates for
context management regardless of the switch).

## Per-model compatibility settings

Every model carries its own compatibility settings (WebUI "Models &
Compatibility" editor → drawer; stored as `models.compat` in the DB):

| Setting | Values | Notes |
|---|---|---|
| `thinking_handling` | `strip` (default for non-Claude) / `map-to-reasoning-effort` / `block` | Non-Claude targets cannot verify Anthropic thinking signatures — `strip` is the safe default; `map-to-reasoning-effort` converts `budget_tokens` into `reasoning_effort` where the target supports it; `block` refuses thinking requests with a protocol-conform error |
| `server_tool_emulation` | `native` / `emulate-firecrawl` / `off` | `emulate-firecrawl` substitutes missing web tools via Firecrawl (§6.2), recorded in route_trace |
| `max_tokens_clamp` | int / off | Clamp `max_tokens` for targets with lower output limits |
| `context_window` | int | Used by capability routing and the count_tokens heuristic |
| `cache_control` | `forward` / `drop` | `cache_control` markers only survive Anthropic→Anthropic; `drop` logs the degradation |

## What degradation looks like (never silent)

Every compat-policy application and every fallback transition is recorded
in the request's `route_trace` (WebUI → "Logs" → request detail). You will
see, e.g.:

```json
{"event": "compat_policy", "setting": "thinking_handling", "value": "strip", ...}
```

Known v1 degradations are documented in the
[translation matrix](reference/translation-matrix.md#known-degradations-v1):
no fabricated thinking signatures, cache markers dropped on
non-Anthropic crossings, heuristic token counting on non-Anthropic-shaped
upstream.

## Why this works (market context)

Anthropic's own docs do not support routing Claude Code to non-Claude
models via a gateway (plan fact C8). Market practice does it anyway —
LiteLLM documents it as a tutorial (plan facts M1/M2), and community
proxies (claude-code-router, …) make it their core feature (M3). The
uniform reported degradation is extended thinking on non-Claude targets,
which the compat policy handles explicitly and visibly.