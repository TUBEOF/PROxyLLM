# ADR-010: Model sync, web-tool provider kind, capability fallbacks

- **Status**: Accepted
- **Date**: 2026-09-17

## Context

Three gaps surfaced operating the WebUI:

1. Models are hand-curated (`models` table, manual WebUI editor). Upstream
   providers add and drop models; the proxy only learns of changes when
   requests start failing. `/v1/models` and the routing rule editor read
   only the local table.
2. Firecrawl — the universal web-tool fallback behind emulated
   `web_search`/`web_fetch` — is configured solely via
   `PROXYLLM_FIRECRAWL_API_KEY` ENV. It has no provider-type card, so it
   cannot be connected, rotated, or self-hosted through the WebUI, and the
   emulator is pinned once at startup.
3. A request requiring vision (image input) whose rule chain holds no
   vision-capable provider fails with `ErrNoRoute`. Web tools have exactly
   one emulator — no failover. Operators want a priority list per
   capability with retry/failover and drag-drop ordering.

## Criteria

- P0: no behavior change without explicit operator configuration; manual
  models and operator edits must survive every automated process; secrets
  stay in the encrypted token store; every transition traced (never
  silent).
- P1: WebUI-first configuration (accounts, lists); live updates without
  restart; no new frontend dependency (200 KB gzip bundle budget).

## Options considered

| Option | Strengths | Weaknesses |
|---|---|---|
| Model sync: replace-on-sync (clobber operator edits) | Simplest state | Destroys compat/capability curation every run |
| Model sync: add-only, no deletes | Zero data loss risk | Dead models linger and keep routing |
| Model sync: delete synced-dropped + preserve edits (chosen) | Upstream truth for synced rows, operator truth for manual rows | Requires provenance column |
| Firecrawl as LLM provider type | Reuses catalog verbatim | Pollutes LLM chains with a non-chat backend |
| Firecrawl as web_tool-kind provider type (chosen) | Full account machinery (keys, settings, cards), chain-validated away | New `kind` field on the catalog |
| Capability fallbacks per routing rule | Max flexibility | Schema migration + heavy UI; overkill for the normal case |
| Capability fallbacks global per capability (chosen) | One place to maintain, simple KV storage | Not per-rule tunable |

## Decision

1. **Model sync (ADR-010 policy: delete + preserve).** Every model row
   records its provenance (`models.origin`: `manual` | `synced`, migration
   v4). A new `ModelLister` provider surface (OpenAI `GET /v1/models`,
   Ollama `GET /api/tags`, codex backend `GET /models` best-effort) feeds
   the syncer: upstream-added models land as `synced` rows, existing rows
   keep operator edits (capabilities/compat/priority) and only refresh
   names, synced rows absent from the upstream list are deleted, manual
   rows are never touched (storage refuses any other origin in bulk
   deletes). A scheduler runs the sync at startup and on the configured
   cadence (config-KV `model_sync_enabled`, `model_sync_interval_hours`,
   state in `model_sync_state`); the WebUI offers toggle + "Sync now".
2. **Firecrawl is a provider type of kind `web_tool`.** The ADR-009
   catalog gains a `kind` field (`llm` | `web_tool`). Firecrawl accounts
   use the standard api_key flow with an optional `base_url` type setting;
   construction yields a `routing.Emulator` instead of a chat provider.
   `validateChain` refuses web_tool types in LLM chains. The ENV key
   remains the fallback when no DB account exists.
3. **Capability fallbacks are global per capability.** Config-KV lists
   (`vision_fallback`, `web_search_fallback`, `web_fetch_fallback`),
   edited on the Routing page via a dependency-free drag-drop PrioList
   (native HTML5 DnD + button fallbacks). The web lists order the
   executor's `EmulatorPool`, which walks an ordered emulator list per
   tool family and fails over on error (trace stays `emulated_tool`;
   all-failed surfaces the last error as the documented "unavailable"
   result).

   **Wiring fix (2026-10-06):** the claim above is now true as written —
   the web lists were stored and validated since their introduction but
   the pool build ignored them (registry insertion order won, and
   `EmulatorPool.SetSearch/SetFetch` had no production caller). Pool
   assembly resolves both lists per saved ACCOUNT order with
   last-known-good degradation; the ENV emulator rides the pool tail.

   **Amendment (2026-09-21, automatic capabilities — supersedes the
   original router-side vision mechanism):** static provider flags cannot
   know which MODEL accepts images, so the pre-stream vision gate was
   removed. Vision fallback entries are now MODEL IDs (routing
   identities, validated against the models table; v1.0.2 provider-ID/
   type entries no longer resolve and are skipped until re-saved). The
   fallback fires EXECUTOR-side, only after the routed chain really
   failed with `capability_gap` rejections (HTTP 400 naming the input
   modality — new FO-2 class, failover action): the fallback models get
   one walk of their own (complete and stream dispatch alike; pre-first-
   byte, FO-1 conform), no recursion. Every consultation stays a traced
   `capability_fallback` event. Rejections LEARN: `vision:false` is
   written into the model row's capabilities blob asynchronously, unless
   the operator pinned the flag (manual selection in the Models editor);
   the row's sync origin is preserved.

## Consequences

- The route_trace schema gains the `capability_fallback` event type
  (version bump to 3); the WebUI renders traces as raw JSON, so no
  renderer change is required.
- `/v1/models` now reflects upstream reality for synced providers; routing
  rules can target upstream models without manual registration. Operator
  rows remain authoritative where they exist (ID collisions are skipped
  with a traced log).
- One dead emulator no longer kills emulated web tools when a second
  account is configured; operators gain real failover by adding accounts.
- Model-sync runs against a codex backend without a list endpoint are
  skipped loudly (typed error → per-provider `skipped` state), never
  failing the whole run.
- Per-rule capability fallbacks remain a possible future extension; the
  KV schema stores plain lists and would move into rule rows unchanged in
  shape if that need materializes.