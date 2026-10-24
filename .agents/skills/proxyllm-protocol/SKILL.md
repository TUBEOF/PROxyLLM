---
name: proxyllm-protocol
description: Use when changing anything wire-shaped in PROxyLLM — mapping fields between OpenAI, Anthropic, Ollama or SystemOne, writing or editing a codec under internal/translate, adding a protocol, handling SSE stream events, or fixing a translation bug.
---

# PROxyLLM protocol layer

Hub-and-spoke: every protocol is exactly one codec pair against the canonical
IR in `internal/translate/ir` — never a pairwise converter. Adding a protocol
means one codec pair plus one streaming state machine per direction;
`internal/translate/{anthropic,openai,ollama,systemone}` are the existing
examples, `internal/translate/sse` is the shared event-stream reader/writer.

## The workflow for any wire change

1. **Re-read the current official documentation** for the field/endpoint
   involved — not memory, not this repo's own output.
2. Update or add the golden fixture under `testdata/golden/` with the source
   URL and access date in `.meta.json`. A fixture encoding a shape the
   reference does not describe is a bug, not a snapshot.
3. Change the codec; keep unknown fields in `Passthrough` (same-protocol
   re-emit, cross-protocol drop with a debug log naming the field path).
4. Derive tests from the fixture and the specification. A test that only
   reproduces current behaviour is a defect.
5. Update `docs/reference/translation-matrix.md` in the same change.

## Field mapping

The per-field truth is `docs/reference/translation-matrix.md` — client surface
× upstream protocol, including the degradations. Never invent a mapping at the
call site.

## Known edges the codecs already handle (verify before "fixing")

| Edge | Where |
|---|---|
| data: URI images decode to base64 ImageParts | `internal/translate/openai/datauri.go` |
| Ollama tool_calls carry OBJECT arguments | `internal/translate/ollama/ollama.go` |
| Thinking signatures ride verbatim, never recomputed | `ir.ThinkingPart` |
| web_search result is an ARRAY, web_fetch a SINGLE object | `ir.ServerToolResultPart` |

## Adding a protocol

One directory under `internal/translate/<name>` with: `DecodeRequest`,
`EncodeRequest` (IR → wire for the upstream), response decode, a stream decoder
and a stream encoder, contract tests against goldens. Register the provider
side via `proxyllm-provider`. A new client surface additionally wires a route
in `internal/server/httpapi` and an `docs/api/` page.

## Verify

```bash
go test ./internal/translate/...
make vet lint test            # check-tests refuses weakened/deleted tests
make test-race                # anything streaming-adjacent
```