# Route trace

Every request writes one log row carrying a `route_trace`: the ordered
record of every routing transition that request made. It is the answer to
"why did this request land there, and what did the proxy do to it?" — and
it is a versioned, public schema, because the console renders it and
operators build tooling on it.

## Shape

```json
{
  "version": 8,
  "events": [
    {"type":"alias","from":"sonnet","to":"claudecode/sonnet"},
    {"type":"implicit_route","model":"claudecode/sonnet","provider_id":"acc-ollama"},
    {"type":"model_resolved","from":"claudecode/sonnet","to":"glm-5.3"},
    {"type":"compat_policy","reason":"thinking_handling=strip-output"},
    {"type":"attempt","provider_id":"acc-ollama"},
    {"type":"emulated_tool_attempt","provider_id":"acc-web","to":"web_fetch","reason":"answered"},
    {"type":"emulated_tool","provider_id":"acc-web","reason":"fetched https://example.com via acc-web"}
  ]
}
```

`version` is additive: a newer runtime may write events an older consumer
does not know, and an older row keeps rendering unchanged — including rows
from before the field existed, which are read as version 1.

## Events

| Type | Meaning | Notable fields |
|---|---|---|
| `alias` | A client-facing alias resolved to its target. At most one per request (a target is never re-resolved) | `from`, `to` |
| `implicit_route` | No rule matched; the model row's provider was used | `model`, `provider_id` |
| `model_resolved` | The routing identity resolved to the upstream wire name | `from`, `to` |
| `context_override` | A Claude Code `[1m]` marker was stripped | `from`, `to` |
| `expand` | A chain entry naming a provider *type* expanded to its accounts | `provider_id`, `reason` |
| `rotation` | A round-robin strategy rotated the candidate list | `reason` |
| `compat_policy` | A compatibility policy was applied (thinking handling, emulation, cache markers, a clamp) | `reason` |
| `skipped` | A candidate was not attempted, with the reason (health cooldown, missing capability, missing credential) | `provider_id`, `reason` |
| `attempt` | One dispatch to one provider started | `provider_id` |
| `retry` | The same provider is being retried after a rate limit | `provider_id`, `reason` |
| `failed` | One attempt failed, classified | `provider_id`, `class`, `error_code`, `http_status`, `failure_position`, `latency_ms` |
| `failover` | The walk moved to the next chain entry | `from`, `to`, `reason` |
| `capability_fallback` | The capability fallback was consulted (an image described, or a fallback rejected) | `capability`, `provider_id`, `reason` |
| `image_caption` | An image was replaced by a description | `provider_id`, `latency_ms`, `usage`, `cache_hit` |
| `emulated_tool` | A server-tool call was answered by a web-tool account | `provider_id`, `to`, `reason` |
| `emulated_tool_attempt` | One account's outcome inside an emulated call (v7) | `provider_id`, `to`, `reason` |
| `rejected` | The request ended without an answer, with the reason | `reason` |

`failure_position` distinguishes a failure **before** the first streamed
byte (`pre_stream`, retryable) from one **after** it (`mid_stream`,
terminal). `class` is the failure classification — `rate_limit`,
`server_error`, `capability_gap`, `context_window_exceeded`, `transport`,
`content_filter`, `insufficient_quota`, `client_error` — documented in the
[error model](../api/errors.md#failure-classification).

## Version history

| Version | Added |
|---|---|
| 1 | The original `{"events":[...]}` shape, retroactively numbered |
| 2 | `expand` and `rotation` (accounts per provider type, strategies) |
| 3 | `capability_fallback` (the capability fallback walk) |
| 4 | `model_resolved` (routing identity → wire name) |
| 5 | `implicit_route` (a row routing its own provider) |
| 6 | `context_override` (the `[1m]` marker strip) |
| 7 | `emulated_tool_attempt` (per-account outcomes in a web-tool walk) |
| 8 | Per-operation facts on the events that describe an operation: token usage, latency, the emulating account, and the caption cache-hit marker |

## Sub-operations come from the trace

Some steps a request takes are worth their own log row: a failover, a
capability fallback walk, an emulated web-tool call. They are **derived
from the trace by one mapping** (the same mapping the console uses), so the
log page and the trace cannot disagree. A derived row inherits the
request's own facts — its protocol and client key — and carries what the
step itself spent.

Two deliberate non-rows: a **same-provider retry** stays a timeline event
(it re-attempts the same operation rather than naming a separable one, and
a retry storm would flood the log), and a caption served from the cache is a
substitution without an upstream call, so it is traced but not logged.

## What a trace never contains

Request or response bodies, headers, credentials or prompts. It records
transitions, classifications and identifiers. Payload capture is a separate,
opt-in feature with its own redaction
([admin API](../api/admin.md#logs)).

## Rendering

The console shows a trace as a chain summary plus a routing timeline, with
the raw JSON behind a disclosure — which is what keeps an unrecognized or
older shape readable instead of invisible.
