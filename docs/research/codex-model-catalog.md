# Codex model catalog wire shape

Read on 2026-11-26 for ADR-023. Codex reads a catalog from
`model_providers.<id>.model_catalog_url` when `[features]
api_key_model_discovery = true` is set, appending `client_version=<its
version>` to the request. The answer is a document with a **top-level
`models` array** — a gateway that answers the OpenAI `{"data":…}` list shape
leaves the picker on Codex's bundled models.

## Fields

| Field | Type | Source |
|---|---|---|
| `slug` | string, required | `codex-rs/protocol/src/openai_models.rs` (`openai_models::Model`); the model id Codex sends |
| `display_name` | string | same |
| `description` | string | same |
| `supported_in_api` | bool | same |
| `visibility` | string (`"list"` shows the model in the picker) | same |
| `priority` | integer (higher = preferred default in the official picker) | same |
| `context_window` / `max_context_window` | integer | same |
| `supported_reasoning_levels` | array of `{effort, description}` | same; **must be present** — codex 0.153 rejects the whole document when an entry omits it (empty array is valid) |
| `default_reasoning_level` | optional string | same |
| `input_modalities` | array of `"text"` / `"image"` | same |
| `use_responses_lite` | bool | same |

Sources: `github.com/openai/codex` (`codex-rs/protocol/src/openai_models.rs`),
`docs.litellm.ai/docs/proxy/client_setup/codex_cli` (top-level `models` key,
`model_catalog_url`, `api_key_model_discovery`), and the public Codex catalog
implementations (`claurst` `CodexCatalogModel`, `sub2api`
`codexCatalogConfig.ts`).

## What PROxyLLM emits

Only the fields the proxy can state truthfully: `slug`, `display_name`,
`description`, `supported_in_api: true`, `visibility: "list"`, `priority`
(row order), `supported_reasoning_levels: []`, `input_modalities` (text, plus
image when the row's capability blob says vision), and `context_window` /
`max_context_window` when `compat.context_window` is set. `use_responses_lite`
is always false — the proxy does not speak the lite shape. Reasoning levels
are not invented: the proxy does not know a model's effort presets.
