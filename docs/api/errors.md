# Errors

Every surface answers failures in its own reference's envelope. The status
codes and the classification behind them are shared.

## Envelopes

**OpenAI-compatible** (`/v1/chat/completions`, `/v1/responses`,
`/v1/decisions`):

```json
{"error":{"message":"…","type":"invalid_request_error","code":"invalid_request_error"}}
```

`code` mirrors `type` for proxy-generated errors; an upstream error body's
own `code` is relayed when there is one.

**Anthropic-compatible** (`/v1/messages`,
`/v1/messages/count_tokens`):

```json
{"type":"error","error":{"type":"not_found_error","message":"model: my-model"}}
```

**System One** (`/v1/systemone`): the reference's flat shape, which has no
error-type field — `{"error":"…"}`.

Messages are redacted before they are written and before they are logged: a
credential that an upstream echoed into its error text comes back masked.

## Model not found (404)

A request 404s when no enabled routing rule matches the model **and** no
model row exists for it. The body is the reference's model-not-found
envelope:

```json
{"type":"error","error":{"type":"not_found_error","message":"model: <the name the client sent>"}}
```

The spelling is deliberate and load-bearing. Claude Code classifies a 404
as model-not-found — the precondition for its own model fallback and
re-selection — only when the error type is `not_found_error` **and** the
message names the model as `model: <id>`. A prose message would read as an
opaque 404 and the client could not recover.

A model that exists but currently cannot serve is a **different** answer —
see the overload case below.

## All providers unavailable (retryable)

When the model routed but every provider in its chain is cooling down — a
circuit breaker or an upstream quota/rate-limit window — the answer is
retryable and surface-specific:

| Surface | Status | Type |
|---|---|---|
| Anthropic | `529` | `overloaded_error` |
| OpenAI-compatible | `503` | `server_error` |
| System One | `500` | — |

Unlike the 404, a client is expected to retry these. The body names the actual
state — `circuit breaker` or an upstream `quota/rate-limit` window — and, when
a deadline is known, the response carries `Retry-After` with the seconds until
the earliest provider recovers. The `route_trace` records the per-provider
`skipped (health cooldown)` events and a terminal `rejected` event naming the
cooldown and its reason.

## Status codes

| Status | Meaning | Retryable |
|---|---|---|
| `400` | Invalid request: body decode failure, a surface policy rejection (`anthropic_surface_non_claude=block`, `thinking_handling=block`) | no |
| `401` | Missing or invalid API key | no |
| `403` | Key present but not permitted (key scope) | no |
| `404` | No route for the model (see above) | no |
| `429` | Upstream rate limit, relayed with `Retry-After` | yes, after the delay |
| `499` | The client closed the request — a log marker, never sent to a client | — |
| `502` | Transport failure to every attempted provider, or a chain whose failures differed | yes |
| `503` | OpenAI surfaces: all providers in cooldown | yes |
| `504` | A dispatch attempt ran out of time (response-header wall), or the request's own budget expired (emulation loop budget, client-side deadline) | yes |
| `529` | Anthropic surface: all providers in cooldown | yes |

## Failure classification

Every upstream failure is classified once, and the classification decides
what happens next. The classes are visible in the `route_trace` per attempt.

**Fails over to the next chain entry** — the request is valid, another
target can serve it:

- `capability_gap` — the target cannot take an input modality (an image
  sent to a text-only model). Recognized from a body code
  (`unsupported_content`, `image_not_supported`) or the message text; only
  on a 400, so a 404 that mentions "image" stays a routing problem.
- `context_window_exceeded` — the input is larger than the target's
  window. Vendors word this in prose, in mutually different phrasings, so
  the matcher reads both the body code (`context_length_exceeded`) and a
  collected set of vendor wordings. Checked *after* the modality tier: a
  vision-less model that also overflows stays a capability gap, because the
  modality is what an operator configured a fallback for.
- `server_error` — 5xx, 529, 408, and statuses a provider cannot explain.
- `transport` — the connection failed, or a deadline expired mid-request.
- `insufficient_quota` — the account is out of credit; it gets its own
  longer cooldown.

**Retries the same provider** — `rate_limit`, honoring `Retry-After`
capped by the backoff ceiling.

**Terminal** — the client must change the request: `content_filter` /
`content_policy_violation`, and any 400/401/403/404/422 that carries no
recognizable body code. A terminal failure ends the request with the
upstream's status; the chain does not keep spending on it.

## Streaming failures

A request-level retry happens **only before the first streamed byte**. Once
bytes are on the wire the client owns the stream: an upstream failure
terminates it with the surface's error event, and there is no mid-stream
provider swap. The terminal error and the full trace land in the request
log beside the row.

A stream that delivers nothing at all for five minutes is cut off by the
transport's idle watchdog (`internal/provider/internal/httpx`); the client
sees a stream error, and the log row names the stalled attempt.

## Web-tool errors

Emulated web tools answer *inside* the response as tool results, not as
HTTP errors — the documented error codes of the family
(`too_many_requests`, `max_uses_exceeded`, `url_not_allowed`,
`url_not_in_prior_context`, `url_not_accessible`,
`unsupported_content_type`, `unavailable`). The request itself still
returns 200 with a model answer that read the error result. A web-tool
backend that is not configured at all produces the `unavailable` result,
so the model can tell the client it could not look it up.

## What is logged

Every request — success, rejection or failure — writes one log row: status,
latency, usage, the client key, the provider that served it, and the
`route_trace`. The row's `error_redacted` column carries the same message
the client saw, after redaction.
