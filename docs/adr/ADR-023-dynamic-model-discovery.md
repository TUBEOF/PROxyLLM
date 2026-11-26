# ADR-023: Dynamic model discovery for the target CLIs

- **Status**: Accepted
- **Date**: 2026-11-26

## Context

The three target CLIs each hard-code a model list. Without a discovery path,
a proxy model (for example `ollamacloud/glm-5.3` or an alias) exists but is
invisible in the pickers, and the operator has to edit a model name by hand
before the first request. The v2.0.0 work order names the exact mechanism for
each CLI.

- **Claude Code** (2.1.269 and later): with
  `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1`, it calls
  `GET {ANTHROPIC_BASE_URL}/v1/models?limit=1000` at start (three-second
  timeout, sends both `Authorization` and `x-api-key`, refuses redirects) and
  expects `{"data":[{"id","display_name","description"}]}`. The client keeps
  only ids that contain `claude` or `anthropic` (case-insensitive). That
  filter is a client fact; the proxy cannot and should not defeat it.
- **Codex**: a provider block may carry `model_catalog_url`, and with
  `[features] api_key_model_discovery = true` Codex calls that URL (appending
  `client_version=<its own version>`) and expects the Codex-native catalog: a
  top-level `models` key, not `data`. A gateway that answers with the OpenAI
  list shape leaves the picker on the built-in models. `model_catalog_json`
  (a local file) is the alternative.
- **OpenCode**: it fetches `/v1/models` only for its built-in providers. For a
  custom provider the native mechanism is a static `models` block in
  `opencode.json` (key = OpenCode id, `modelID` = upstream id). A community
  discovery plugin exists but is not required.

## Criteria

- P0: a model configured in the proxy is selectable in each CLI without
  hand-editing a model id.
- P0: no protocol shape changes for existing clients (additive fields only).
- P1: one discovery endpoint per distinct wire shape; no endpoint invented for
  a CLI that does not read it.
- P1: honest documentation where the client filters or does not discover.

## Options considered

| Option | Strengths | Weaknesses |
|---|---|---|
| Do nothing; document model ids | Zero code | The picker stays useless; the largest usability gap remains |
| Rewrite model ids to satisfy Claude Code's filter | The filter passes | Hides the real routing target; breaks OpenAI clients; magic |
| Additive `/v1/models` fields + Codex catalog + installer-generated OpenCode blocks (this ADR) | Each CLI gets its native mechanism; no cloaking | Three mechanisms to document and test |

## Decision

1. **Anthropic `GET /v1/models`** stays the Claude Code discovery source. It
   gains `display_name` and `description` from the model row, appended without
   changing `data[].id`. The docs and the installer recommend a routing alias
   whose id contains `claude` or `anthropic` (for example
   `anthropic/glm-5.3`) when the model must appear in Claude Code's picker.
   This is a naming convention, not proxy-side renaming: the proxy never
   disguises an id.
2. **`GET /v1/model_catalog`** is new: pk-key auth on the OpenAI family,
   answering the Codex-native shape with a top-level `models` array. The
   installer writes `model_catalog_url` into `~/.codex/config.toml` and turns
   on `api_key_model_discovery`. The exact field set is taken from the
   codex-rs models manager at implementation time and recorded in
   `docs/research/codex-model-catalog.md` with a citation — a fixture built
   from a guess is a defect, not a snapshot.
3. **OpenCode** gets no new endpoint. The installer reads the proxy's
   `/v1/models` and generates the static `models` blocks.

## Consequences

- A new handler, OpenAPI entry, golden fixture (catalog shape) and an M1 cell
  that validates the response against the catalog schema; a separate test
  keeps the OpenAI `/v1/models` shape untouched.
- Codex needs `api_key_model_discovery` as well as `model_catalog_url`; the
  installer and the docs carry both, and an M2 cell exercises the picker path.
- Claude Code's substring filter is documented as a client limitation with the
  alias recipe, not "fixed" in the proxy.
- The installer stays the only place that generates static OpenCode model
  blocks. The proxy does not duplicate its own `/v1/models` under a second
  path for a client that would not read it.

## Sources

- Claude Code gateway discovery (`CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY`,
  `/v1/models?limit=1000`, the substring filter) — code.claude.com
  LLM-gateway discovery documentation, transcribed in the v2.0.0 work order;
  re-verify the flag and shape before implementation.
- Codex catalog (`model_catalog_url`, `[features] api_key_model_discovery`,
  the top-level `models` key, `client_version`) —
  `docs.litellm.ai/docs/proxy/client_setup/codex_cli` and openai/codex
  configuration references, accessed 2026-11-26; field names re-verified
  against codex-rs at implementation time.
- OpenCode custom providers and static `models` blocks — opencode.ai/docs,
  accessed 2026-11-26.

