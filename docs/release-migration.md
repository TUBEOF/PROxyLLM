# Upgrading from v1 to v2.0.0

v2.0.0 narrows the product to eight provider types and three client protocol
families. It is a **breaking** release: removed types and surfaces stop
working, and their configuration is not migrated.

## Removed

| Removed | Replacement |
|---|---|
| `gemini` provider and the `/v1beta/*` client surface | Use `custom-openai` against an OpenAI-compatible endpoint, or an Anthropic/OpenAI client with a routing rule |
| `openai-responses` provider type | `custom-openai` (the Responses **client** surface stays — Codex uses it) |
| `openai-decisions` provider, `/v1/systemone`, `/v1/decisions` | None — decision models are out of scope |
| The 9 extra M2 CLI adapters (aider, cline, crush, goose, gptme, interpreter, kimi, openhands, qwen, continue) | Those tools still work through the OpenAI-compatible surface; they are no longer in the automated matrix |

Provider rows of a removed type are not rewritten. The registry logs a skip
naming the row; delete the row in the console and create the replacement.

## What to do

1. **Before upgrading**, list your providers and note any of the removed
   types: `GET /api/admin/providers`.
2. For each removed row, create a `custom-openai` provider pointing at the
   same upstream root and copy the model rows to it, or delete the row if the
   upstream is no longer used.
3. Remove routing rules that name the removed provider type or accounts.
4. Re-create aliases that pointed at decision models; those targets no longer
   exist.
5. Clients: the OpenAI, Anthropic and Ollama surfaces are unchanged. Clients
   that called `/v1/systemone`, `/v1/decisions` or `/v1beta/*` must move to
   one of those surfaces.

## New in v2.0.0

- **Anthropic OAuth (Teams/Enterprise)** — `anthropic-oauth`, browser sign-in
  with a pasted callback, versioned risk notice in the console.
- **Dynamic model discovery** — `display_name`/`description` on the Anthropic
  model list, and `GET /v1/model_catalog` for Codex.
- **Auto-installer** — `scripts/install.sh` for Claude Code, Codex and
  OpenCode.
- **Connect surfaces** — the `/connect` page, a key-dialog disclosure and a
  wizard step.
- **Test hardening** — golden-update guard, header/latency/disconnect cells,
  fake IdP, contributor compose stack.
- **CI gates** — ShellCheck, installer end-to-end, OpenAPI contract diff.

## Rollback

v2 does not migrate the database schema; a v1 binary can open a v2 database
(the provider type column is free-form). Removed provider rows simply stop
constructing in v2. Take a backup before upgrading anyway.
