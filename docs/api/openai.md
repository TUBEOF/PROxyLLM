# OpenAI-compatible API

Auth: `Authorization: Bearer pk_…`. Paths: `POST /v1/chat/completions`,
`POST /v1/responses`, `GET /v1/models`.

## `POST /v1/chat/completions`

The OpenAI Chat Completions shape, streaming (`"stream": true`, SSE with
`data:` chunks terminated by `data: [DONE]`) and non-streaming.

Accepted on the request: `model`, `messages`, `tools` / `tool_choice`,
`max_tokens` / `max_completion_tokens`, `temperature`, `top_p`, `stop`,
`stream`, and image parts (`image_url`, including `data:` URIs). Unknown
fields pass through to the upstream untouched.

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H 'content-type: application/json' \
  -d '{"model":"ollamacloud/glm-5.3","messages":[{"role":"user","content":"hi"}]}'
```

Three wire details that matter when the client is an agent loop:

- **An assistant turn with only tool calls is encoded with `"content": ""`,
  never `null`.** Ollama's compatibility endpoint rejects null content with
  `400 invalid message content type: <nil>`; genuine OpenAI accepts both and
  both decode to the same empty content on the way back.
- **A system message inside `messages` is carried as a system message** —
  not dropped. Agents append instructions mid-conversation (hook output,
  injected reminders) and the upstream must see them.
- **Tool-call arguments that arrive as one complete delta are queued onto
  the block they belong to**, not dropped. Some upstreams emit
  `function.arguments` as a single finished string in the same delta that
  opens the call; the accumulated result is identical either way.

Reasoning is not a Chat Completions field, and upstreams disagree on how to
send it: `delta.reasoning` (Ollama), `delta.reasoning_content` (DeepSeek-style
on the stream), `message.reasoning_content` (the same providers,
non-streaming). The decoder accepts all three and buffers interleaved
fragments, so an upstream that alternates reasoning and text still produces
one coherent reasoning block followed by the text — never text sliced into
fragments by stray reasoning bytes.

## `POST /v1/responses`

The OpenAI Responses shape, with the semantic event stream:
`response.created`, `response.output_text.delta`, `response.output_item.*`,
`response.completed` (usage on the terminal event). Field-level translation
between this shape and Chat Completions — including what cannot be
represented — is in the
[translation matrix](../reference/translation-matrix.md).

```bash
curl http://localhost:8080/v1/responses \
  -H "Authorization: Bearer $KEY" -H 'content-type: application/json' \
  -d '{"model":"ollamacloud/glm-5.3","input":"hi","stream":true}'
```

## `GET /v1/models`

The OpenAI list shape — `{"object":"list","data":[{"id","object","owned_by"}]}`
— with one entry per model row, `owned_by` naming the provider. The list is
never `null`; an empty store answers an empty array.

```json
{"object":"list","data":[{"id":"ollamacloud/glm-5.3","object":"model","owned_by":"acc-ollama"}]}
```

**One path, two shapes:** when the request carries an `anthropic-version`
header, this same path answers the Anthropic discovery shape instead — see
[the Anthropic page](anthropic.md#get-v1models). A client that sets that
header by habit (some SDK configurations do) gets the other shape.

A model that is not in this list can still route: a model row is what makes
a name known, and rows are created by the model sync or by hand in the
console.
