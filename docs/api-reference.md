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

## Admin API (WebUI backend, session-authenticated)

Base path `/api/admin/`, JSON, validated. Bootstrap creates the first
admin account when none exists.

| Group | Endpoints |
|---|---|
| Sessions | `POST /bootstrap`, `POST /login`, `POST /logout` |
| Provider types (ADR-009) | `GET /provider-types` (catalog: labels, form field descriptors, auth kind), `GET /provider-types/{type}` (settings + accounts), `PUT /provider-types/{type}/settings`, `POST /provider-types/{type}/accounts` (server-minted id; api keys sealed in the token store; oauth drafts validate against the type settings' issuer) |
| Providers | `GET /providers`, `PUT /providers/{id}`, `DELETE /providers/{id}`, `POST /providers/{id}/oauth/start`, `POST /providers/{id}/oauth/poll` |
| Models | `GET /models`, `PUT /models/{id}`, `DELETE /models/{id}` (compat settings per model) |
| Routing | `GET /routing`, `PUT /routing/{id}`, `DELETE /routing/{id}` — rules carry a per-rule `strategy` (empty = inherit); chain entries name provider types, provider ids, or `provider/model` pairs; `GET /routing-strategy`, `PUT /routing-strategy` (global default, live-swap) |
| Client keys | `POST /keys` (secret shown once), `GET /keys`, `POST /keys/{id}/revoke` |
| Observability | `GET /usage`, `GET /logs` (redacted; `route_trace` detail) |
| Settings | `GET /settings`, `PUT /settings`, `DELETE /settings/{key}` (DB override layer) |

Live-routing overrides via settings: `model_aliases`,
`anthropic_surface_non_claude`, `routing_strategy` — a successful write
reaches the runtime without restart (see
[configuration.md](configuration.md#database-overrides-layer-4)).

`route_trace` rows carry `"version":2` since ADR-009 (additive: the new
`expand` and `rotation` event types; older rows have no version field and
render unchanged).

## Health

| Endpoint | Notes |
|---|---|
| `GET /healthz` | Unauthenticated health probe (also used by the container healthcheck via the in-binary `-healthcheck` flag) |