# Anthropic-compatible API

Auth: `x-api-key: pk_…` and/or `Authorization: Bearer pk_…` (`Bearer` wins
when it carries a well-formed key). Paths: `POST /v1/messages`,
`POST /v1/messages/count_tokens`, `GET /v1/models`.

The Messages shape is what Claude Code speaks, so this surface is the one
that makes a non-Claude model usable from the CLI. The
[Claude Code guide](../claude-code.md) covers the client side of that.

## `POST /v1/messages`

Streaming (SSE) and non-streaming. The event set is the reference's:
`message_start`, `content_block_start`, `content_block_delta`,
`content_block_stop`, `message_delta`, `message_stop`, and `ping`.

Streaming relay rules:

- **Events are relayed as they arrive.** Pings included — the client's
  liveness logic depends on them.
- **An upstream that sends a stream for a non-streaming request is not
  tolerated**: the surface decodes what the request asked for.
- **Unknown event or delta types do not fail the stream.** They are
  re-emitted as an SSE comment line, so a new provider-side event type
  cannot break a conversation.
- **No mid-stream provider swap.** Once bytes are on the wire, the client
  owns the stream; an upstream failure ends it with an error event.
- A stream that delivers nothing at all for five minutes is cut off by the
  transport's idle watchdog.

`?beta=true` is accepted on the path and the query string rides through:
`anthropic-version` and `anthropic-beta` headers are forwarded to the
upstream verbatim, never filtered against an allowlist.

```bash
curl http://localhost:8080/v1/messages \
  -H "x-api-key: $KEY" -H 'content-type: application/json' \
  -d '{"model":"claudecode/sonnet","max_tokens":128,
       "messages":[{"role":"user","content":"hi"}]}'
```

### Per-model behaviour on this surface

The target model's compatibility policy decides how the request is adapted
before dispatch. Every adaptation is recorded in the `route_trace`, never
silent:

- **Thinking** — `thinking_handling` governs the `thinking` parameter and
  thinking blocks in history: strip, map to a reasoning effort the target
  understands, or reject the request outright. A target that did not ask
  for thinking never receives a thinking block; unsolicited reasoning an
  upstream emits is dropped (a client that reads the first content block as
  the answer would otherwise see an empty reply).
- **`cache_control` markers** — forwarded verbatim on an
  Anthropic→Anthropic route; dropped on every other route, with a debug log
  naming the field.
- **`max_tokens`** — clamped when the operator configured a clamp for the
  target, and only then: the proxy never guesses a model's output limit.
- **Context window** — the model row's window feeds the capability filter
  and the token estimate; requests that would overflow fail over to a
  target that can take them.

The per-model settings themselves are documented once in
[model compatibility](../reference/model-compat.md).

### The `[1m]` context marker

Claude Code appends a marker to a model name to ask for a 1M-token context
(`claude-sonnet-4-6[1m]`). It is a **client-side hint, never part of the
model identity**: routing strips it (`[1m]` / `[1M]`, case-insensitive) and
dispatches the base model, recording a `context_override` event in the
trace. Write routing rules and aliases against the **base** name — a
pattern that includes the marker never matches.

## `POST /v1/messages/count_tokens`

Answers an Anthropic token-count response:

```json
{"input_tokens": 1234, "estimation": "heuristic"}
```

The count is a **local estimate** (characters ÷ 4 over the decoded text),
marked `estimation: "heuristic"` — it never claims to be an exact
tokenizer count, because the proxy does not run one. The request performs
no upstream dispatch, but it is still logged.

This endpoint is deliberately **not** governed by the
`anthropic_surface_non_claude` switch: it neither routes nor reaches a
model, and a tool that only measures a payload should not be blocked by a
surface policy.

## `GET /v1/models`

With an `anthropic-version` header this path answers the Anthropic
discovery shape:

```json
{"data":[{"id":"claudecode/sonnet","type":"model","display_name":"sonnet"}],
 "first_id":"claudecode/sonnet","last_id":"claudecode/sonnet","has_more":false}
```

Entries are ordered by model ID ascending, which is what fixes
`first_id` / `last_id`. Without the header, the same path answers the
[OpenAI list shape](openai.md#get-v1models).

## Claude Code configuration

```bash
export ANTHROPIC_BASE_URL="http://localhost:8080"
export ANTHROPIC_AUTH_TOKEN="pk_…"      # the client key
export ANTHROPIC_MODEL="claudecode/sonnet"   # any model row or alias
```

Claude Code's auxiliary requests (the WebFetch summarizer, the auto-mode
classifier) use pinned model IDs that change with every Claude Code
release, which is why the alias map supports wildcards — see
[Claude Code with non-Claude models](../claude-code.md).
