# ADR-014: Image-to-text vision fallback

- **Status**: Accepted
- **Date**: 2026-10-10
- **Supersedes**: the context-window gate of ADR-013

## Context

ADR-010 (amended 2026-09-21) gave the proxy an executor-side vision fallback:
when the whole rule chain failed with `capability_gap` rejections — a 400 naming
the input modality — the configured `vision_fallback` models got a walk of
their own. ADR-013 then made that walk size-aware, because the fallback
**re-sends the whole conversation**, so a fallback model smaller than the
request could never serve it; the executor skipped such entries before
dispatching them.

That gate was a fix for the wrong axis. The rescue exists for a long agentic
session that grows an image part mid-conversation — a screenshot to analyse.
That is precisely the case where the conversation is far larger than any
configured vision model. Gating on size means the rescue is unavailable exactly
when it is needed: with every configured fallback smaller than the history, the
request still fails, now with an honest reason instead of a hard failure.

The mistake was structural, not a tuning problem. Re-sending the history makes
the fallback model's context window a precondition of the rescue; the window of
the model doing the *describing* has no business being one.

## Criteria

- **P0 — never silently degrade**: every substitution, skip, and classification
  is recorded in `route_trace` and visible in the log.
- **P0 — the rescue must not invent**: the answering model may only be told what
  a vision model actually saw. A description produced without the image is worse
  than no description, because downstream it is indistinguishable from a real one.
- **P1 — the rescue must work at any conversation size**: history length must not
  be able to disqualify it.
- **P1 — the operator's routing must survive**: the model that answers stays the
  routed one; the rescue restores that routing rather than replacing it.
- **P2 — reuse what exists**: the image part, the protocol codecs, the provider
  registry and the trace machinery are all already there.

## Decision

**Describe the images; do not re-send them.**

When a chain exhausts on `capability_gap` rejections and the request holds image
parts, the executor:

1. **Extracts** every image part — descending into tool-result content, which can
   carry images too (`ir.Request.ImageRefs`).
2. **Describes** each one with the configured vision fallback models, in order,
   until one answers. Each model receives **only the image and the instruction**
   — no conversation, no history, no tools (`ir.Request` fragment). Its context
   window therefore never enters the question, and ADR-013's gate has nothing
   left to gate.
3. **Substitutes** each description for its image, leaving the client's own text
   untouched (`ir.Request.ReplaceImages`).
4. **Re-dispatches the same chain** with the substituted request. The model that
   answers is the one the operator routed to. One pass only: the re-dispatch runs
   with the fallback disabled, so a model that rejects the transcript too ends the
   request instead of starting a second round.

The instruction is task-aware by default: the client's most recent text turn is
appended so the description targets the question that came with the image
(a generic description of a screenshot reports the window chrome and misses the
error line), and visible-text transcription is called out, because a screenshot
is usually attached for the text in it.

### Boundaries

- **A per-image failure keeps that image in place.** No placeholder, no dropped
  input. The request is then unchanged and still valid for a vision-capable
  attempt, and the worst case is an unanswered request, never a silently
  impoverished one.
- **A refused image is refused loudly.** A remote image is downloaded and inlined
  before the describer sees it — wire encoders differ, and at least one drops a
  URL-sourced image outright, which would leave the vision model describing
  something it never received. The fetch goes through the guarded HTTP client
  (private ranges and redirects refused) and is size-capped.
- **The caption spend is billed.** Token usage of the caption calls is folded
  into the request's usage, so the client's accounting does not under-report.
- **`ClassContextWindow` (`context_window_exceeded`) stays.** It is right on its
  own terms: a 400 saying the input exceeded the model's window is a valid
  request against too small a model, and it fails over rather than terminating.

## Consequences

- A long session that grows a screenshot is rescued by any configured vision
  model, whatever its context window. The rescue no longer has a size precondition.
- `ChainEntry.MaxContext`, `gateFallbacks`, and the max-context wiring in
  `resolveCapabilityFallbacks` are gone: they existed only to soften the resend.
  `storage.ContextWindowFromCompat` remains — the window is still a detected fact
  about a model row, read by the model sync; it simply no longer decides rescues.
- Two new trace surfaces, both required by P0: an `image_caption` event (the
  describer and the description's length) and, for a substitute failure, a
  `capability_fallback` event naming the reason. `image_caption` is also a child
  op, so the log gets a row per description.
- The OpenAI-surface data-URI defect is fixed as a precondition. Decoding kept a
  `data:` URI whole in `ImagePart.URL` while leaving `Data` empty, so the encoder
  rebuilt it as `data:;base64,` — every OpenAI-surface image lost its bytes in
  transit. The Anthropic surface carries base64 natively and never hit it, which
  is why it went unnoticed. Without the fix the describer would have been handed
  an empty payload.
- Descriptions are cached nowhere. A caption replaces the image in the request
  the client re-sends, so an agentic session pays one description per image per
  request. A caption cache is the obvious next step if that cost matters; it is
  not needed for correctness.

## Addendum 2026-10-11: the learning gets a reader, and the operator gets a veto

The operator reported: "as soon as an image is in the context, it is carried
along on every new request and permanently triggers a vision fallback". The
cause was that the learning described above was **write-only**. The executor
wrote `vision:false` on every `capability_gap` into the model row — correctly,
transactionally, honoring `pinned` — and **no production path ever read it
back**. `GetModelByProviderAndName`, the lookup authored for exactly this, had
no caller outside its own tests. So every turn re-sent the image, the upstream
rejected it again, and the description walk ran again. The consequences note
above ("one description per image per request") was describing the symptom.

**The gate.** Resolution now drops a candidate when its model row *knows* it
cannot see images and the request carries one. Two properties keep it from
being a bigger decision than it is:

- A row with no opinion is **permissive**. This is what preserves the
  original no-pre-stream-gate design, and its stated reason — provider-level
  capabilities cannot know which model accepts images — is exactly what
  changed: a *model row* can know, because it learned it.
- It **never empties a chain**, and it only runs when there is a choice to
  make. Every candidate being known-blind is precisely the case this ADR's
  description walk exists for; that request must still be dispatched and
  rejected for the rescue to fire. Emptying the chain there would have turned
  a rescue into a hard no-route.

So it is a preference among survivors, applied last, not a filter.

**The veto already existed.** `pinned` is written by the Models editor, not by
the server: the editor sends the map itself, `pinned[flag] = on`, so checking a
capability pins it and unchecking it removes the pin — which is what makes
"uncheck it and the learning resumes" true. An earlier draft of this change
added a server-side re-derivation of `pinned` from the boolean flags in the same
save; that pinned the *unchecked* flags too and inverted the editor's contract,
so it was dropped. The stored blob rides through verbatim, as before.

**Also fixed on the way through:** the model row's capability blob carries the
`pinned` **object** beside its boolean flags, and two readers decoded it
straight into `map[string]bool` — which fails outright on that object. Both
therefore read as "no capabilities" for every row that had ever learned
anything, i.e. exactly the rows in active use. `capabilityFlags` silently
stripped thinking handling from them; the new vision lookup would have been
blind in the same way. Both now parse in two steps and treat only a real
boolean as a flag.

**Still not cached.** A conversation whose *whole* chain is known-blind still
pays one description per turn: with nothing better to route to, the rescue is
the only way to serve the request at all. A caption cache keyed by image
content is the step that would remove that cost; it is deliberately not in this
change.

## Addendum 2026-10-14: the description is computed once

The cost the paragraph above names was real and the operator kept paying it. A
client re-sends its own transcript on every turn, image parts included, so a
conversation that carried a screenshot re-described the same bytes again and
again for as long as it lived. Descriptions are now cached, keyed on the image's
decoded bytes.

Three properties decide whether such a cache works at all, and each is the
opposite of the obvious:

- **The key uses the base prompt, never the composed one.** The instruction the
  describer receives appends the client's latest turn, so keying on what was
  actually sent would miss on every turn — the trap that makes a caption cache
  cost more than it saves.
- **What is cached is the finished replacement text**, not the bare description,
  so the substituted position stays byte-identical turn over turn. An upstream
  prompt cache matches on an exact byte prefix; a history position whose text
  changes between turns invalidates everything after it.
- **The key carries the client key.** The describer is shown that client's
  question as a steering hint and its answer can quote what it was shown, so a
  shared bucket would let one client's wording ride into another's request.

A failure is never cached, and a failed description still leaves the original
image in place.

**The rescue now runs before the dispatch when it can.** If every candidate's
model row has learned it cannot see images and every image already has a
description, the images are replaced before the chain is walked: the rejection
that would have triggered the rescue is a known outcome rather than a discovery,
and paying a round trip for it buys nothing. The reactive path stays for the
first turn, which is what teaches the model row in the first place.

**A cache hit writes no log row.** The sub-operation log lists operations, and a
description that was already known is the absence of one. The substitution is
still recorded — as a trace event, in the request's own detail view — because a
description that silently replaced an image would be exactly the kind of
unobservable behaviour this ADR exists to prevent.
