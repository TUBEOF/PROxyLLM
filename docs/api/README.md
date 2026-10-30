# Client API

PROxyLLM serves four client-facing surfaces from one endpoint. They share
authentication, request limits and error conventions; each speaks its own
protocol family on the wire.

| Surface | Paths | Streaming | Auth header |
|---|---|---|---|
| OpenAI-compatible | `POST /v1/chat/completions`, `POST /v1/responses` (HTTP or WebSocket upgrade), `GET /v1/models` | yes | `Authorization: Bearer pk_…` |
| Anthropic-compatible | `POST /v1/messages`, `POST /v1/messages/count_tokens`, `GET /v1/models` | yes | `x-api-key: pk_…` and/or `Authorization: Bearer pk_…` |
| Ollama native | `POST /api/chat`, `GET /api/tags` | yes (NDJSON) | `Authorization: Bearer pk_…` and/or `x-api-key: pk_…` |
| Decision | `POST /v1/systemone`, `POST /v1/decisions` | no — by contract | either family above |

Per-surface detail lives on its own page: [OpenAI](openai.md),
[Anthropic](anthropic.md), [decisions](decisions.md). The error model is
shared and documented once in [errors.md](errors.md). `GET /v1/models` is a
single path whose response shape depends on the surface — see below.

The operator-facing admin API (`/api/admin/*`, session-authenticated) is a
different audience and has its own page: [admin.md](admin.md).

## Authentication

Every client request on these surfaces carries an **API key**
(`pk_<prefix>_<secret>`), created in the console (**API keys**) and shown
exactly once. The key is stored as a one-way keyed hash, not as a recoverable
secret — a lost key is regenerated or deleted, never re-read.

The Anthropic surface accepts both header families: `Authorization: Bearer`
wins when it carries a well-formed key, otherwise `x-api-key` is read. The
OpenAI surface reads `Authorization: Bearer` only.

Authentication is on by default. `PROXYLLM_AUTH_DISABLED=1` turns it off
with a loud startup warning; it exists for local experiments, not for
anything reachable.

## Shared request conventions

- **JSON only**, `Content-Type: application/json`; responses are JSON or
  SSE (`text/event-stream`).
- **Request bodies are capped at 32 MiB.** A larger body is a client error,
  not a truncation.
- **Unknown fields pass through.** Both surfaces forward unknown body
  fields and headers instead of filtering them against an allowlist:
  `anthropic-version` and `anthropic-beta` ride to the upstream verbatim, so
  a beta feature the client opts into is not silently dropped by the proxy.
- **No proxy-side rate limiting.** The proxy does not throttle clients;
  a per-key limiter would have to guess a budget the operator never set.
  An upstream 429 passes through with its `Retry-After` honored for
  same-provider retries.
- **Model names in responses echo what the client asked for.** A routing
  identity (`ollamacloud/glm-5.3`) stays the client's string; the wire name
  the upstream expects never leaks into a response field.

## Shared response conventions

- **Usage** is normalized per protocol: input/output tokens, cache
  creation/read tokens where the protocol has them, and the web-request
  counters (`server_tool_use.web_search_requests` /
  `web_fetch_requests`) an emulated web tool actually spent.
- **Every request produces a log row**, including rejected ones — status,
  latency, usage, and a `route_trace` describing every routing transition.
  The trace schema is documented in
  [route-trace.md](../reference/route-trace.md).
- **Failures are never silent.** A fallback, a capability substitution or a
  dropped field is recorded in the trace; the client sees a
  protocol-conform error or the real answer.

## Compatibility promise

There is no API version header: the client surfaces track the reference
implementations of the protocols they speak. The `route_trace` schema *is*
versioned and additive — an older row renders unchanged, and a consumer
that does not know a newer event can ignore it.

## Route index

| Method | Path | Page |
|---|---|---|
| `POST` | `/v1/chat/completions` | [OpenAI](openai.md#post-v1chatcompletions) |
| `POST` | `/v1/responses` | [OpenAI](openai.md#post-v1responses) |
| `GET` | `/v1/responses` | WebSocket upgrade for the Responses transport |
| `GET` | `/v1/models` | [OpenAI](openai.md#get-v1models) / [Anthropic](anthropic.md#get-v1models) |
| `POST` | `/v1/messages` | [Anthropic](anthropic.md#post-v1messages) |
| `POST` | `/v1/messages/count_tokens` | [Anthropic](anthropic.md#post-v1messagescount_tokens) |
| `POST` | `/api/chat` | Native Ollama chat (JSON or NDJSON) |
| `GET` | `/api/tags` | Native Ollama model list |
| `POST` | `/v1/systemone` | [Decisions](decisions.md#post-v1systemone) |
| `POST` | `/v1/decisions` | [Decisions](decisions.md#post-v1decisions) |
| `GET` | `/healthz` | [Deployment](../deployment.md) |
