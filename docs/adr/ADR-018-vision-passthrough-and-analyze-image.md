# ADR-018 — Vision pass-through and the analyze_image tool

**Status:** Accepted (2026-10-18)
**Supersedes:** none. **Extends:** [ADR-014](ADR-014-image-to-text-vision-fallback.md)
(image-to-text captioning), [ADR-015](ADR-015-web-tool-emulation-resilience.md)
(emulation degrade contract).

## Context

ADR-014's image-to-text rescue answers a chain of models that cannot see
images by describing the image ONCE, with a prompt composed from the client's
question, and handing the routed model second-hand prose. Two problems remain:

1. **The description is the only chance.** The routed model gets a
   2–3-sentence caption instead of the pixels. A model working on a
   Playwright screenshot needs to ask again — "what is the exact error
   string?" — and cannot: the image is gone, and a new question means a new
   caption in a new turn, never inside the one it is working in.
2. **The reactive trigger fails silently on a hung upstream.** The rescue
   engages only when EVERY chain attempt classifies as `capability_gap` — a
   400 naming the modality. An upstream that accepts the connection and then
   produces no response headers (the observed `http2: timeout awaiting
   response headers` against `ollama.com`) turns into a `transport`-class
   failure instead. The class is legitimate failover input, but it masks the
   gap the rescue waits for: the image request dies with
   `chain exhausted: Post ...: http2: timeout awaiting response headers`
   and the rescue never had its chance.

The requirement behind the fix is behavioral: a pasted image, in a turn that
must analyze it (paste, or a tool-produced screenshot) must let the model
send **the image plus its own prompt** to a vision model — any question it
needs, repeated, within the request — while the proxy stays invisible to the
client (Claude Code must notice nothing).

## Decision

### 1. Pass-through stays the default

When the resolved chain holds any entry whose vision capability is unknown
or positive, an image-bearing request dispatches **unchanged** with its
image content blocks, no captioning, no tool. The router's preference gate
(ADR-014's non-emptying learned gate) already orders known-blind candidates
behind sighted ones; neither the gateway nor the executor strips anything.
The reactive ADR-014 rescue remains the safety net for an entry that looks
able but rejects after all.

### 2. All-known-blind chains substitute BEFORE dispatch

When every chain entry's model row says `vision:false` (learned or pinned),
the executor replaces every image pre-dispatch — the rejection is certain in
advance and must not be paid, and on a hung upstream it may never even
arrive (that is problem 2). Per image:

- the placeholder text renders from the `vision_placeholder_template`
  KV (default
  `[Bild {n} (ref {ref}): {caption} — call the analyze_image tool with image_ref "{ref}" and your own prompt to inspect this image]`),
  where `{n}` is the image's document ordinal, `{ref}` the content id below
  and `{caption}` the bootstrap description;
- the bootstrap description comes from the caption infrastructure — cache
  first, else the configured `vision_fallback` models — and stays in the
  request the client re-sends, so it is paid once.

**Deviation from ADR-014, deliberate:** a per-image caption failure no
longer keeps the image in place on THIS path. ADR-014's invariant was written
for chains that might still contain a sighted attempt; on an all-blind chain
the raw image is certain to be rejected, so a failed caption substitutes
`image content not available` into the `{caption}` slot instead. The tool is
injected either way, answers `unavailable` in-band while no describer works,
and the conversation continues.

### 3. Injected analyze_image tool

The executor appends an emulated tool definition to the request's tools —
executor-side, after compat and chain resolution, so every surface (Anthropic,
OpenAI, Ollama, decision models) gets the same conforming behavior and no
wire codec needs to know it:

- name `analyze_image`, input `{image_ref: string, prompt: string}`
  (both required), `max_uses: 8`;
- `image_ref` is the **12 hex chars of the SHA-256 of the image's decoded
  bytes** — the same digest the caption cache is keyed on. It is stable
  across turns because the bytes travel with the re-sent transcript, and
  identical for an inline payload and a resolved-then-inlined URL of the
  same picture. The ordinal / `Bild N` form also resolves;
- the model's call is answered with the image + the model's OWN prompt sent
  to the `vision_fallback` list (the same describer walk the captions use:
  first working model answers), the answer's spend folded into the
  request's usage, and the answer cached under the same rules the caption
  cache holds — `image bytes + the ask`, so an identical ask is free and a
  changed ask re-describes (the 2026-10-17 rule extended to tool asks);
- bounds mirror the web-tool emulation exactly: `max_uses` 8,
  `EmulationCallBudget` per call, `EmulationTotalBudget`/`maxEmulationRounds`
  for the loop.

**The client never sees the tool.** Unlike the web tools (which claim
client-known names), `analyze_image` is *added*: its calls are answered
inline and its blocks never enter the final response — on any surface.

**Mixed turns and loop ends:** when the model emits an `analyze_image` call
AND a client tool call in one turn, the vision call is dropped (traced,
reason "not answered: mixed with client tool calls") and the client's own
calls surface unchanged — an unknown `tool_use` must never reach the client.
The same drop rule applies wherever the loop ENDS without answering:
round cap reached or the emulation deadline exhausted (a degraded vision
fallback invites a model to keep retrying its ask). A client that itself
declares a tool named `analyze_image` owns the name: no def is injected and
no call of theirs is intercepted. The tool description instructs
one-call-at-a-time so this corner stays rare. Should a model habitually pair
them, the extension is to answer the vision call first and surface only the
client's calls — open door, not built.

### 4. Streaming trade

A request on an all-blind chain with images (and any request with emulated
defs) rides the COMPLETE emulation loop, so the response is a synthesized
stream: no token streaming for this request class. The documented trade from
the web tools applies unchanged (`docs/routing-and-fallbacks.md`), and
`vision_tool=0` degrades the whole feature to plain caption substitution
(the executor's pre-018 cache-only path, extended with network captions).

### 5. The timeout root cause

The shared transport's response-header wall (`http2: timeout awaiting
response headers`) is now operator-configurable via
`upstream_response_header_timeout_ms` (default `30000`, applied per provider
load). Raising it keeps a slow-but-correct multimodal upstream serving
instead of failing into a transport class — and proactive substitution on an
all-blind chain means the image never reaches a blind model to hang on
either way.

## Trace

`route_trace` v9 adds the `analyze_image` event (answering provider + model,
usage, latency, ask/answer summary). The substitution itself rides the
existing `capability_fallback` event; the individual captions ride
`image_caption` as before. A denied mixed-corner call traces an
`analyze_image` event with the decline reason. A log child row of kind
`analyze_image` (with drawer payload) derives from it — the redaction
discipline is the caption one, because the answer is model output derived
from an image.

## Consequences

- Claude Code pasting an image at a text-only routed model gets a first-turn
  answer built from a short caption plus the ability to interrogate the
  image — the proxy remains invisible (no new client-side tool, no unknown
  block shapes).
- Operators configure the behavior through two new settings-KV keys —
  `vision_tool` and `vision_placeholder_template` — in addition to the
  existing `vision_caption_*` trio and `vision_fallback` (documented in
  `docs/configuration.md`).
- The `vision_fallback` list now serves two consumers (pre-dispatch
  substitution/answers and the reactive bootstrap) with one contract: model
  IDs, first working model wins.
- No storage migration: the substitution is per-request, the image bytes
  never persist beyond it.
- **Known limitation (accepted): a chain whose rows are all known blind AND
  tools-incapable gets the tool injected into a request its upstream could
  reject for carrying tools (`requiredCapabilities` is computed pre-injection
  at the surface, so the router does not know). Pre-018 the same request was
  served from the caption alone. Rare cell; the follow-up is a tools flag
  lookup beside ModelVisionLookup and skipping the injection the same way
  `vision_tool=0` does.**
