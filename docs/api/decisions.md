# Decision API

Decision models are a third operation class beside chat: instead of
generating text, they score **typed questions against shared evidence in one
forward pass**. Cloudflare's `clef` (served by Ollama as `clef:cloud`) and
OpenAI's Decisions models are the members of the class. Shared properties:
non-streaming by contract, no tools, no message history — a state plus
questions, answers out, billed on input tokens.

PROxyLLM exposes both client shapes and translates between them, so an
Ollama-shaped client can reach an OpenAI-shaped decision upstream and the
other way round. Field-level mapping is in the
[translation matrix](../reference/translation-matrix.md).

## `POST /v1/systemone`

The System One / Jev shape. `state` is the evidence — a nonempty string, or
an object/array serialized as JSON text. `questions` is an **object keyed by
question name**; the key is the answer's name.

```bash
curl http://localhost:8080/v1/systemone \
  -H "x-api-key: $KEY" -H 'content-type: application/json' \
  -d '{
    "model": "clef",
    "state": "Our checkout has returned 500 errors since 9am.",
    "questions": {
      "label":    {"type":"choice","instructions":"Which label fits this ticket?",
                   "criteria":{"billing":"Payments and refunds","bug":"Software errors"}},
      "urgent":   {"type":"noul","instructions":"Is this blocking a customer right now?"},
      "severity": {"type":"score","instructions":"How severe is the impact?",
                   "criteria":["No impact","Workaround available","Feature broken"]}
    }
  }'
```

The three question types are `choice` (a `criteria` object; ties follow the
model's option order), `noul` (a yes/no predicate, answered as a
probability), and `score` (an ordered `criteria` array, lowest first).

The answer mirrors the request's shape: `answers` keyed by the question
name, each with its type, the picked `choice` or `score`, a `probabilities`
map, and a `confidence`. A score answer additionally carries a `legend`
mapping its numeric levels back to the criteria strings.

```json
{"model":"clef:cloud",
 "answers":{"label":{"type":"choice","choice":"bug",
                     "probabilities":{"billing":0.0125,"bug":0.9781},"confidence":0.8906},
            "urgent":{"type":"noul","noul":0.92}},
 "usage":{"input_tokens":174,"output_tokens":1}}
```

`keep_alive` (a duration string or seconds) is accepted and passed through
on this surface only.

## `POST /v1/decisions`

The OpenAI Decisions shape. `input` replaces `state`, and `questions` is an
**array**; the question's `name` is the answer's name. `choice` questions
carry `choices: [{"value","description"}]`, `score` questions
`levels: [{"label","description"}]` in ascending order, and `predicate` is
the class's yes/no type.

```bash
curl http://localhost:8080/v1/decisions \
  -H "Authorization: Bearer $KEY" -H 'content-type: application/json' \
  -d '{
    "model": "clef",
    "input": "The build is red and the last commit touched the auth layer.",
    "questions": [
      {"type":"choice","name":"cause","instructions":"Which area is the cause?",
       "choices":[{"value":"auth"},{"value":"docs"}]}
    ]
  }'
```

```json
{"model":"clef",
 "answers":[{"type":"choice","name":"cause","choice":"auth",
             "probabilities":[{"value":"auth","probability":0.9955},
                              {"value":"docs","probability":0.0045}],
             "confidence":0.982}],
 "usage":{"input_tokens":130,"output_tokens":0}}
```

Both surfaces accept images (`images`), which attach to every question in
request order — a bare base64 string on systemone, a data URL on the
Decisions shape. Questions are scored **independently**: an answer is never
fed into a later question.

## Routing

A decision request routes by model with the `decision` capability required:
a chain entry whose provider does not declare it is filtered before
dispatch, and the executor skips an implementation that does not serve the
class at all. Ollama Cloud accounts declare it by default (the same account
credential serves `/v1/systemone`); everything else is the operator's pin.

The **model row** additionally records the registry's per-model fact: the
model sync stores `"decision": true` for a model whose discovery entry
declares the decision surface. That is documentation of the model, not a
second gate — routing stays with the account, so a detection failure can
never silently take a capable model out of service.

## Documented losses

The IR carries both shapes; what one surface cannot express is a
client-visible outcome, never an invention:

- A `score` **level description** (OpenAI) has no slot in systemone's
  string-per-level `criteria` — it is dropped crossing that way.
- A **refusal** answer (OpenAI-only) has no systemone shape: the systemone
  surface answers `502` naming the refused question instead of inventing an
  answer.
- **Usage**: systemone documents it as required, but an upstream that
  reports none leaves the proxy omitting the object rather than emitting
  zeros.
- **`keep_alive`** exists on systemone only and is dropped crossing to the
  Decisions shape.
- An image crossing into the Decisions shape needs a media type, which the
  systemone wire does not carry: it is sniffed from the bytes, and an
  unrecognizable signature fails the request with a clear error rather than
  guessing.

## Logging

Decision requests write the same log row as chat requests, with
`protocol` set to `systemone` or `decisions` and the `decision` feature in
`features`. Payload capture is not wired for this class: the row, the
`route_trace` and the usage are.
