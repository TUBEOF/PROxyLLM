# ADR-021: Provider and surface focus for v2.0.0

- **Status**: Accepted
- **Date**: 2026-11-26

## Context

PROxyLLM ships eleven provider types and six client surfaces. Each additional
type or surface costs a codec pair, golden fixtures, matrix cells, JSON-schema
validation, an API page, a console card and fixture refreshes whenever a vendor
drifts. The v2.0.0 scope is stability and minimalism: three coding CLIs
(Claude Code, Codex, OpenCode) plus any OpenAI-compatible client, over one
canonical IR.

No CLI in that set speaks Gemini natively, and none speaks the OpenAI
Decisions API or the System One surface. Those paths have no consumer in the
target deployment, yet they own tests, schemas and documentation that must be
kept honest on every release.

The provider types that remain each have a concrete consumer: Ollama Cloud
(chat and web tools), Codex OAuth, Anthropic API key, Anthropic OAuth
(ADR-022), Firecrawl, Custom OpenAI and native Ollama. `custom-openai` is what
makes "any OpenAI-compatible client" true; native Ollama is the same product
family as Ollama Cloud and shares its wire basis, so it costs almost nothing.

## Criteria

- P0: every target client runs end-to-end against every supported surface.
- P0: no capability loss for the three target CLIs.
- P1: smaller code, fixture and matrix surface.
- P1: one codec pair per protocol in the IR hub — the architecture does not
  change, the set of protocols does.

## Options considered

| Option | Strengths | Weaknesses |
|---|---|---|
| Keep all eleven types and six surfaces | No migration; existing users unaffected | Keeps paying maintenance for unused paths; vendor drift keeps forcing fixture work |
| Hide the unused paths in the UI, keep the code | Smaller visible surface; reversible | Codecs, fixtures and matrix cells still rot; tests still pin shapes nobody uses |
| Remove the unused paths (this ADR) | Smaller surface; honest tests and docs; simpler IR | Breaking release; old rows and old configs stop working; migration page required |

## Decision

The supported provider set in v2.0.0 is exactly:

`ollama-cloud`, `ollama-cloud-web`, `codex-oauth`, `anthropic` (API key),
`anthropic-oauth` (ADR-022), `firecrawl`, `custom-openai`, `ollama-native`.

Removed:

- the `gemini` provider **and** the Gemini client surface (`/v1beta/*`);
- the generic `openai-responses` provider type (the OpenAI Responses *surface*
  stays — it is Codex's home surface);
- the `openai-decisions` provider, the `/v1/systemone` and `/v1/decisions`
  surfaces, the System One codec and the decision operation class in the IR.

The Anthropic `count_tokens` endpoint stays; it belongs to the Anthropic
surface and has nothing to do with decisions. `google-genai`-shaped golden
fixtures and their schemas are deleted with the surface.

The IR keeps one operation class (chat) instead of two. Routing loses the
decision walk and the decider interface; the rule chain, strategies, health,
capability fallbacks and the web-tool emulation loop are unchanged.

Old provider rows of a removed type are not migrated. The provider registry
logs a skip with a pointer to the WebUI, which is the existing behaviour for
rows that cannot be constructed; the migration page names the removed types
and the replacement (`custom-openai` or an API key against a supported
surface).

## Consequences

- Breaking change → v2.0.0 with `docs/release-migration.md`.
- Code, tests, fixtures, schemas, matrix cells and API pages are cut in the
  same change; a grep for the removed names must return only migration and
  history mentions, and those are listed in the PR.
- ADR-017 (decision models) is superseded by this ADR: the operation class it
  introduced is removed. The identifier stays stable and its file stays for
  history.
- The storage layer keeps the free-form provider type string, so no migration
  is needed and a future type stays a data change, not a schema change.
- The translation matrix and the ADR index diagram shrink in Phase 7; they are
  not rewritten piecemeal in the removal change.

## Sources

- v2.0.0 work order, Anhang A (provider and surface target matrices),
  2026-11-26.
