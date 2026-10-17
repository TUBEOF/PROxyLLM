# ADR-017: Decision models — a third operation class on the IR hub

- **Status**: Accepted
- **Date**: 2026-10-16

## Context

A new model class arrived that is not a chat model: **decision models**
(also "System One" / TypeSafe's Jev). Cloudflare's `clef` (27B) is served
by Ollama as `clef:cloud` through `POST /v1/systemone` — a state plus typed
questions (`choice`/`noul`/`score`), scored in **one forward pass**;
OpenAI's Decisions API (`POST /v1/decisions`, beta, `gpt-6-luna`) carries
the same idea with `predicate`/`choice`/`score`. Anthropic has no
counterpart. Shared facts of the class: **non-streaming** ("streaming,
video, tools, and generation controls are not supported"), no tools, no
messages, billed on **input tokens only**.

PROxyLLM's IR (ADR-004) knew one operation: chat (`Request`/`Response`).
Routing, the executor, the log row and the WebUI all assumed a chat
request. Nothing in the tree could carry "questions and typed answers".

## Criteria

1. One canonical hub (ADR-004): N client surfaces × M upstream dialects
   must stay N + M codec pairs, never N × M converters.
2. Routing integration for free: chains, strategies, health, capability
   gating, wire-name resolution, the request log — reuse, not a parallel
   stack.
3. Honest translation: a shape a target cannot represent fails loudly or
   omits, never fabricates.
4. Minimal blast radius on the chat path.

## Options considered

- **Squeeze decisions into the chat IR** (one user message in, text out):
  rejected — typed answers, images-per-question semantics and usage
  semantics have no chat representation; the proxy would parse generated
  prose back into types, an invention.
- **Per-surface pass-through without translation**: rejected — Ollama
  clients and OpenAI clients would each be pinned to a matching upstream;
  two surfaces × two upstreams collapses into four converters, the exact
  N × M shape ADR-004 exists to prevent. The maintainer's call was
  explicitly the bidirectional translation.
- **A second IR operation pair beside `Request`/`Response`** (chosen).

## Decision

1. **IR**: `ir.DecisionRequest`/`ir.DecisionResponse` (in `translate/ir`)
   with the shared vocabulary: state, images, typed questions
   (`choice`/`score`/`predicate`), answers (choice/probability/score,
   probability lists, confidence), usage, the systemone-only `keep_alive`.
2. **Codecs**: one pair per wire — `translate/systemone` (Ollama/Jev) and
   `translate/openai` (`DecisionsCodec`, the OpenAI shape) — both
   translating through the IR in both directions. Either client surface
   reaches either upstream.
3. **Capability**: `Capabilities.Decision` is the routing gate (provider
   pins, `knownCapabilities`, the routing filter, the log row's `features`
   JSON, the WebUI flags); `provider.Decider` is the implementation
   contract: `Decide(ctx, ir.DecisionRequest) (ir.DecisionResponse, error)`.
   Ollama Cloud declares the flag by default (the same account credential
   serves `/v1/systemone`).
4. **Routes**: `POST /v1/systemone` and `POST /v1/decisions` on the client
   surface, behind client-key auth like every `/v1/` route. The pipeline is
   the chat pipeline minus compat and streaming: decode → route
   (`decision` required) → wire-name resolution → `Executor.ExecuteDecision`
   → encode → log row (`protocol`: `systemone` / `decisions`).
5. **Dispatch**: decisions are single-shot per entry — no same-provider
   retry, failover to the next chain entry, health reported per attempt,
   every transition traced (attempt/failed/failover, the chat vocabulary).

## Consequences

- **Documented translation losses** (translation-matrix): systemone holds
  one string per score level, so a Decisions `level.description` is not
  representable there; the systemone response's `usage` is documented
  required but a Decisions upstream reports none — the proxy omits it
  rather than fabricating zeros; `keep_alive` exists only on systemone and
  is dropped elsewhere; a `refusal` (OpenAI-only) has no systemone shape,
  so the systemone surface answers 502 instead of inventing an answer; an
  image crossing into the Decisions shape needs a media type — it is
  sniffed from the bytes (bare base64 carries none) and the translation
  fails clearly when the signature is unrecognizable.
- **No streaming, ever**: both references forbid it; no stream codec pair
  exists and none is planned for the class.
- **Payload capture** (the Logs-page conversation/answer view) is not wired
  for decisions; the row, trace and usage are.
- The chat path is untouched: no chat codec, executor or surface changed
  behavior — the decision pipeline reuses their seams.
