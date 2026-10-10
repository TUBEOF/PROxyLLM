# API Reference

All endpoints require authentication unless stated otherwise. Client keys
(`pk_<prefix>_<secret>`) authenticate the model surfaces; WebUI sessions
authenticate the admin API. Auth opt-out only via
`PROXYLLM_AUTH_DISABLED=1` (loud startup warning).

## Client-facing surfaces

### OpenAI-compatible (auth: `Authorization: Bearer pk_...`)

| Endpoint | Notes |
|---|---|
| `POST /v1/chat/completions` | Streaming (`stream: true`) and non-streaming; tools, vision parts |
| `POST /v1/responses` | Semantic streaming events (`response.created`, `output_text.delta`, `response.completed`, …) |
| `GET /v1/models` | Model list (`object: list` with `data` array) |

### Anthropic-compatible (auth: `x-api-key` and/or `Authorization: Bearer`, per C5)

| Endpoint | Notes |
|---|---|
| `POST /v1/messages` | Streaming SSE per the Anthropic event set (`message_start`, `content_block_*`, `message_delta`, `message_stop`, `ping`); `?beta=true` path matching included |
| `POST /v1/messages/count_tokens` | Native passthrough for Anthropic-shaped upstream; heuristic (marked) otherwise; **not** governed by the `anthropic_surface_non_claude` gate |
| `GET /v1/models` | Gateway model discovery |

### Behavioral notes

- **Unknown body fields / headers pass through** (open-list rule, C3):
  `anthropic-version` and `anthropic-beta` are forwarded verbatim, never
  filtered by allowlist.
- **Streaming relay** is as-it-arrives — pings are relayed; the proxy
  aborts a stream with no relayed bytes for 300 s (C2).
- **`cache_control`** is forwarded verbatim only on Anthropic→Anthropic
  routes; otherwise dropped with a debug log (C4).
- **Unknown SSE event types** pass through unparsed-but-logged (A5).
- **Non-Claude models on the Anthropic surface** follow the
  `anthropic_surface_non_claude` switch and per-model compat policy — see
  [Claude Code with non-Claude models](claude-code-with-non-claude-models.md).

## Error model

Protocol-conform per surface:

- **OpenAI surface**: `error.type` / `error.code` / `error.message` with
  the documented status codes (400/401/403/429/500/503); `Retry-After`
  honored upstream.
- **Anthropic surface**: `error.type` / `error.message` per A13;
  upstream overload maps to 529 semantics (`overloaded_error`).
- **Failover**: a request-level retry happens only before the first
  streamed byte; once streaming, upstream failure terminates with a
  protocol-conform error event (no mid-stream provider swap). The terminal
  chain error plus the full `route_trace` land in the request log —
  fallbacks are never silent.
- **Unknown model (404)**: a request 404s when no enabled routing rule
  matches the model AND no models-table row exists (ADR-011). A listed
  model with no rule routes implicitly to its row's provider — traced as
  `implicit_route`.
- **All providers in health cooldown (529/503)**: when the model routed
  but every chain provider is in circuit-breaker cooldown, the Anthropic
  surface answers `529` `overloaded_error` and the OpenAI surfaces answer
  `503` — both retryable, unlike the 404 above. The route_trace records
  the per-provider `skipped` (health cooldown) events and a terminal
  `rejected` event naming the cooldown as the cause.

## Admin API (WebUI backend, session-authenticated)

Base path `/api/admin/`, JSON, validated. Bootstrap creates the first
admin account when none exists.

| Group | Endpoints |
|---|---|
| Sessions | `POST /bootstrap`, `POST /login`, `POST /logout` |
| Account | `GET /me` (the session admin's `{username}` — the avatar menu's identity), `PUT /password` (`{"current_password","new_password"}`; verifies Argon2id, enforces min. 8 chars, revokes every OTHER session of the admin — the requesting session stays; a wrong current password or a too-short new one is the uniform 400) |
| Provider types (ADR-009) | `GET /provider-types` (catalog: labels, form field descriptors, auth kind), `GET /provider-types/{type}` (settings + accounts), `PUT /provider-types/{type}/settings`, `POST /provider-types/{type}/accounts` (server-minted id; api keys sealed in the token store; oauth drafts validate against the type settings' issuer) |
| Providers | `GET /providers`, `PUT /providers/{id}`, `DELETE /providers/{id}`, `POST /providers/{id}/oauth/start`, `POST /providers/{id}/oauth/poll` |
| Models | `GET /models`, `PUT /models/{id}`, `DELETE /models/{id}` (compat settings per model; rows carry the ADR-010 `origin` provenance) |
| Routing | `GET /routing`, `PUT /routing/{id}`, `DELETE /routing/{id}` — rules carry a per-rule `strategy` (empty = inherit); chain entries name provider types, provider ids, or `provider/model` pairs; `GET /routing-strategy`, `PUT /routing-strategy` (global default, live-swap) |
| Capability fallbacks (ADR-010) | `GET /capability-fallbacks`, `PUT /capability-fallbacks` — one write for all three lists (`vision`, `web_search`, `web_fetch`; vision entries = provider IDs/LLM types, web entries = web-tool account IDs, validated at the boundary) |
| Model sync (ADR-010) | `GET /model-sync` (config + last-run state), `PUT /model-sync` (`enabled`, `interval_hours`), `POST /model-sync/run` (synchronous pass, returns the run summary) |
| Client keys | `POST /keys` (secret shown once), `GET /keys`, `POST /keys/{id}/revoke` |
| Observability | `GET /usage`, `GET /logs` (redacted; `route_trace` detail; rows carry `conversation_id`, `protocol`, `features`, `error_redacted` since the Logs-page redesign, plus `key_name`/`key_prefix`/`provider_name`/`provider_kind` display projections), `GET /logs/stream` (SSE, see below) |
| Settings | `GET /settings`, `PUT /settings`, `DELETE /settings/{key}` (DB override layer) |

### Sub-operations (2026-10-07)

A request's separable upstream operations get their own log rows rather
than living only inside the parent's `route_trace`: a chain failover, a
capability (vision) fallback model walk, and an emulated web-tool call
(`web_search`/`web_fetch`). They are derived from the parent's trace by one
mapping (`internal/routing` `ChildOps`) and stored beside it. A
same-provider retry is deliberately **not** a row — it re-attempts the same
operation rather than naming a separable one, and a retry storm would flood
the log with rows; it stays a timeline event in the detail drawer.
`GET /api/admin/logs` returns sub-operations as `children`, a map keyed by
the parent row id (as a string — JSON object keys are strings):

```json
{ "logs": [...], "total": 3,
  "children": { "4": [ { "id": 7, "parent_id": 4, "ts": 1760000000,
                         "kind": "failover", "from_ref": "prv-codex/glm-5.3",
                         "to_ref": "prv-ollama/llama3.3-70b", "detail": "capability fallback model",
                         "from_name": "Work account", "to_name": "Ollama key 1",
                         "provider_name": "Ollama key 1", "provider_kind": "Ollama Cloud" } ] } }
```

`children` is omitted entirely when no row in the page has any
sub-operation; `kind` is one of `failover`, `capability_fallback`,
`emulated_tool`. Sub-operations are **not** requests: they never appear in
`logs` and never count toward `total`, so pagination is unchanged. The
`children` read is batched for the whole page. The `*_name` / `*_kind`
fields are display projections of the raw `*_ref` / `provider_id` values (a
failover ref is a `provider/model` composite, resolved on its provider
half): the admin API resolves them at read time from the provider catalog.
The raw refs remain on the wire — a ref naming something that is not a
configured account (a model id, or a since-deleted row) projects no name
and its raw value stays authoritative.

### Sub-operation payloads (2026-10-10)

A sub-operation has a captured body of its own, so a row can be opened and
read instead of only counted:

```
GET /api/admin/log-payload?owner=child&id=<child row id>
```

Same response shape as the request payload (`found`, `capture_enabled`,
`client_request`, `client_response`, `bytes`, `truncated`). The two sides are
the display projection of what that step sent and received:

- `emulated_tool` — the tool call (`{"tool", "input"}`) and the result the model
  was handed (`{"result": ...}`), re-embedded as JSON rather than an escaped
  string.
- `image_caption` — the instruction the describer received and the image it was
  given by media type and decoded size (`{"instruction", "image": {"media_type",
  "bytes"}}`), and the caption text. **Never** the image bytes: an image is
  recorded by type and size, exactly as in the request payload.
- `failover` — no body (a failover names its reason and does nothing else).

Both sides pass through the same redactor every other log site uses, and each is
capped at 64 KB. A caption is model output derived from an image, and an image is
often a screenshot of a terminal, so a transcribed credential is masked before it
is stored. Payloads are stored only while body capture is on (see
`request_logging_enabled` below) and are dropped, not queued, under write
backpressure. The `image_caption` row's `detail` is a short excerpt of the
caption — the full text is the payload.

### Live log stream (2026-10-06)

`GET /api/admin/logs/stream` is an SSE feed over the request log: the wire
is `event: hello` (ack) → `event: running` (the in-flight snapshot — every
snapshot replaces the previous list) → `event: log` (one finished request
row per event, via the store tail) with `: ping` comment heartbeats every
15 s. The response declares `Content-Type: text/event-stream` (a native
`EventSource` fails the connection on any other type) with `Cache-Control:
no-cache` and `X-Accel-Buffering: no` so intermediaries do not buffer
frames. Rows emitted here carry the same display projections as the page
endpoint (`key_name`, `key_prefix`, `provider_name`, `provider_kind`), so a
row delivered live renders identically to the same row after the next page
fetch. GET needs no CSRF header (the session cookie suffices — native
`EventSource` connects directly). A slow or dead client is dropped at the
first failed flush; `EventSource` auto-reconnect restores state. Note: the
connect keeps the admin session warm (an open Logs tab refreshes the idle
window — a documented side effect, not a bug). Conversation identities
(`conv_<8 hex>`) derive only for requests written after this release —
older rows render without one.

Live-routing overrides via settings: `model_aliases`,
`anthropic_surface_non_claude`, `routing_strategy`, plus the ADR-010
`vision_fallback` / `web_search_fallback` / `web_fetch_fallback` lists —
a successful write reaches the runtime without restart (see
[configuration.md](configuration.md#database-overrides-layer-4)). Since
2026-09-21 the `vision_fallback` entries are MODEL IDs validated against
the models table.

`route_trace` rows carry `"version":6` since the context-marker work
(additive: version 6 added the `context_override` event — the Claude Code
`[1m]` marker strip; version 5 ADR-011's `implicit_route`, version 4 the
`model_resolved` event; version 3 added ADR-010's `capability_fallback`
event, version 2 the ADR-009 `expand`/`rotation` events — older rows render
unchanged, as do unversioned ones). The WebUI renders the trace as a chain
summary plus a routing timeline, with the exact JSON behind a "Raw JSON"
disclosure — so an unrecognised or older shape still renders there.

### Model test (2026-09-22)

| Endpoint | Notes |
|---|---|
| `POST /api/admin/models/test/{id}` | One minimal "Sag OK" round trip against the model's provider: 16-token budget, no thinking, no stream, 15 s timeout. Response `{ok, latency_ms, reply, error}` — failures carry a plain-language, redacted cause; `ok:false` is still HTTP 200 (the round trip happened). Unwired (no master key) answers 404. |

## Health

| Endpoint | Notes |
|---|---|
| `GET /healthz` | Unauthenticated health probe (also used by the container healthcheck via the in-binary `-healthcheck` flag) |