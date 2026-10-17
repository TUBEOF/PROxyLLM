# ADR-013: Context-window-aware fallbacks

- **Status**: Accepted — **superseded in part by ADR-014** (2026-10-10)
- **Date**: 2026-10-09

> **Superseding note (ADR-014).** The context-window **gate** described here
> (`gateFallbacks`, `ChainEntry.MaxContext`) is gone. It existed only because the
> capability fallback re-sent the whole conversation to a fallback model, which
> made that model's window a precondition of the rescue. ADR-014 replaces the
> resend with image description, so the vision model receives one image and the
> window no longer decides anything. What survives from this ADR is the
> **`context_window_exceeded` error class**: a 400 naming an over-long input is
> still a valid request against too small a model, and still fails over rather
> than terminating.

## Context

The capability fallback (ADR-010, amended 2026-09-21) rescues a request whose
target model rejected an input modality: when the whole rule chain failed
with `capability_gap`, the executor re-dispatches **the same, unmodified
request** against the configured fallback models. Only `req.Model` changes.

That design carried an unexamined assumption: that the fallback model can
hold the conversation. It cannot, in the case the fallback exists for. The
canonical trigger is a long agentic session — a Claude Code session on a
1M-token model — that grows an image part mid-conversation (a Playwright
screenshot to analyse). The primary rejects the image; the fallback model
has, say, a 128k window while the conversation is 400k tokens.

Two independent defects then compounded:

1. **The fallback walk had no size awareness.** `resolveCapabilityFallbacks`
   resolved a stored model ID to `(provider, wire name)` and nothing else.
   The model row's context window *was* already detected and persisted
   (`models.compat.context_window`, written by the model sync from Ollama
   `/api/show`) and *was* already loaded per request into
   `routing.Compat.ContextWindow` — where no code read it. `missingCapabilities`
   never checked it either, so the `max_context` capability ADR-005
   documents as a capability **did not exist in code**.

2. **A context-window rejection was terminal.** A 400 saying "prompt is too
   long" matched no error class and fell through to `classifyByStatus(400)`
   → `client_error` → `ActionTerminal`. Inside the fallback walk
   `ActionTerminal` returns immediately, so the walk **died on the first
   fallback model**: a second, larger fallback further down the list was
   never tried, and the client received a 400 it had not caused, replacing
   the informative capability-gap error.

The net effect was a rescue path that converted a recoverable request into a
hard failure precisely in the scenario it was built for.

How other gateways handle this (surveyed 2026-10-09), grouped by approach:

| Approach | What it does |
|---|---|
| Context-window fallback list keyed on the error | A list of its own, selected by a substring match on the error; pre-call checks drop candidates whose counted input exceeds their declared input limit, and raise the same error when every candidate is dropped. No automatic trimming — context compaction is opt-in. |
| Endpoint filtering by declared context length | Providers are filtered before dispatch by their advertised window; the lossy middle-out transform is an explicit, opt-in plugin. |
| Status-keyed fallback only | Any non-2xx by default, no size check. |
| Retryable-provider-errors only | 400/404/422 count as "request validation" — no failover at all. |
| L7 retry | Retry on status/connect-failure; no body inspection. |
| 429/5xx retry only | A 400 passes straight through to the client. |

Two patterns emerge: **A** — a status-keyed static fallback list that re-sends
the request untouched (most of the surveyed gateways) — and
**B** — a pre-dispatch filter that drops candidates which cannot serve the
request (counted-input pre-checks, endpoint filtering by declared length). Pattern
B is the one that keeps a long conversation off a small model; pattern A
alone, applied to images, is the least defensible combination, because the
constraint it ignores (the conversation may exceed the fallback's window) is
structural.

## Criteria

- **P0 — never silently degrade**: every skip, gate, and
  classification is recorded in `route_trace`.
- **P0 — no data loss**: the proxy must not truncate, summarise, or rewrite
  the conversation on its own.
- **P1 — the rescue must actually rescue**: the walk must reach a fallback
  model that can serve the request when one is configured.
- **P1 — fail honestly**: when no configured model can serve it, the client
  must see the *reason*, not a side effect.
- **P2 — reuse what exists**: the window is already detected and persisted;
  the token estimator already exists for `count_tokens`.

## Options considered

| Option | Strengths | Weaknesses |
|---|---|---|
| Status quo (pattern A) | No code change | Structurally cannot serve a long session; converts recoverable requests into hard failures |
| Trim the conversation for the fallback (pattern C) | Makes any fallback model usable; matches the "analyse this screenshot" intent | Lossy and silent by nature — violates P0/P1; the opt-in compaction available elsewhere explicitly refuses to strip to the latest turn, because it changes the request's meaning |
| Image-to-text substitution (vision model describes the image, text model answers) | Localises the vision call to the image, so history size stops mattering; the most complete answer | A second upstream call, a synthesised prompt with its own semantics, and a new class of "what the model saw" to record — too large to land safely in this change |
| Classify context overflow as its own class; gate the fallback walk by the declared window (patterns B, minimal) | Uses only facts already stored; no data loss; every decision traced; makes the walk reach a model that can serve it | Only as good as the detected window (providers that report none stay ungated) |

## Decision

**Adopt the minimal pattern-B form**: context-window overflow becomes its own
error class that fails over, and the capability-fallback walk skips entries
whose declared window cannot hold the request.

Concretely:

1. **`ClassContextWindow` (`"context_window_exceeded"`)**: a 400 whose body
   code (`context_length_exceeded`) or message names an input longer than the
   model's window → `ActionFailOver`. Consulted *after* the modality tier, so
   a rejection naming both stays a `capability_gap` — the modality is what the
   operator configured a fallback for. Mid-stream stays terminal: once
   bytes are relayed, the request is not retried.
2. **`ChainEntry.MaxContext`**: the model row's declared window, resolved by
   the wiring layer from the same compat blob the sync writes. Zero means
   undeclared and is never a reason to skip — the estimate is a lower bound
   and a discarded working fallback is worse than one wasted attempt the new
   class fails over.
3. **The gate runs before the walk**, and when it filters out every entry the
   executor returns the **original** capability-gap error rather than an empty
   exhaustion.
4. **No trimming.** The conversation is never rewritten for a fallback.

The estimator (`ir.Request.EstimateTokens`) moved from `httpapi` into the IR
package so the `count_tokens` surface and the gate cannot drift apart.

## Consequences

- A long session that grows an image now reaches a fallback model that can
  hold it, or fails with the truthful reason (`capability_gap`, plus a traced
  skip per rejected fallback model).
- The documented `max_context` capability now exists in the one place it can
  be evaluated with a real token count. The primary rule chain still does not
  filter on it: `provider.Capabilities` carries no window, so gating the chain
  itself would mean plumbing per-model windows into the router. That remains
  open, and the new class is what keeps it safe in the meantime — an
  over-long attempt against a small primary now fails over instead of
  terminating.
- **Image-to-text substitution is the acknowledged follow-up.** It is the
  only design that serves the screenshot when *every* configured fallback
  model is smaller than the history, and it is where this ADR's third option
  should be revisited.
- Providers that report no window (`custom-openai` endpoints today) stay
  ungated; their fallbacks rely on the new class, and a failed attempt still
  costs one upstream round trip.
