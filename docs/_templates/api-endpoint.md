# API endpoint template

A section for `docs/api/<surface>.md` — one block per endpoint. Existing
examples: `docs/api/openai.md`, `docs/api/anthropic.md`, `docs/api/admin.md`,
`docs/api/decisions.md`. Endpoint shapes follow `docs/api/README.md`'s shared
conventions (auth, errors, streaming); link to them instead of restating.

Delete this header and the skeleton you do not fill.

## `METHOD /path`

One paragraph: what the endpoint does and who calls it (client surface or
operator console). State the auth it requires — link to `README.md` when it
is the shared scheme.

### Request

Table per body field. Types are the wire types, not the Go/TS types.

| Field | Type | Required | Meaning |
|---|---|---|---|
| `field` | string | yes | what it does; the default lives in the Default column only if the wire accepts its absence |

A fenced example body — minimal, every field one that appears in the table
above. No hypothetical fields: the example is derived from the golden
fixture (`testdata/golden/…`) and the official documentation, and its fields
must exist on the wire.

### Response

Same shape as Request. Streaming endpoints: the event sequence in order, one
fenced block, and a link to the streaming conventions in `README.md`.

### Errors

The endpoint's own errors (the ones it can produce that the shared scheme
does not cover), each with the condition that produces it. Everything else
is a link to `errors.md`.