# Routing & fallbacks

The routing engine is deterministic and observable. Every skip, retry,
failover, capability substitution and strategy rotation is recorded in the
request's `route_trace`, so the path a request took can be reconstructed
after the fact — fallbacks are never silent. The event schema and its
versioning are in [route trace](reference/route-trace.md); the console
renders the trace on the request row and derives that request's
sub-operations from it ([the console](operating.md#logs)).

## Model identity

A model's routing identity is `<prefix>/<wire-name>` (`ollamacloud/glm-5.3`).
The prefix is a per-provider-type setting, defaulting to the type string; the
wire name is what the upstream expects. The models table stores both — the ID
is the routing identity, the name is the wire name — and the model sync
creates prefixed rows. Rows without a prefix keep working, their wire name
being their ID.

Before dispatch, a resolved chain's model — a routing identity — is mapped to
the wire name the upstream expects: a model row for that identity is
authoritative, and without one the tail after the last slash is used. Every
entry of a chain carries the same model, so one lookup covers the whole
chain. The client-facing response echoes the routing identity the client
asked for, and the wire name never appears in a response field
([client API](api/README.md#shared-request-conventions)); the resolution is
recorded as a `model_resolved` trace event.

Aliases resolve before rule matching, so an aliased request is routed by its
target's rules. The map is the `model_aliases` setting
([configuration](configuration.md#database-overrides-layer-4)); a key is an
exact name or a `*` pattern, an exact entry wins, and among matching patterns
the one with the most literal characters wins, ties going to the
lexicographically smaller key — exactly one deterministic answer. The
resolution is traced like any other transition, and an alias target is never
resolved again. Wildcard keys exist for clients that fire auxiliary requests
under versioned model IDs pinned by their own releases; the alias setup for
Claude Code is in [Claude Code with non-Claude
models](claude-code.md#model-names-aliases-and-wildcards).

A request that matches nothing answers the reference's model-not-found 404,
whose message names the model ([errors](api/errors.md#model-not-found-404)).

## Rule model (evaluation order)

1. **Key scope** — the client key's allowed model patterns. A key with no
   entry is unrestricted; a request outside its key's patterns is rejected
   ([errors](api/errors.md#status-codes)).
2. **Alias** — the requested name maps to its configured target, which is
   what everything below matches on. The compatibility policy is keyed by the
   same landed name: the surface resolves it from the alias target's row, never
   from the client-typed name — which, for a Claude Code auxiliary ID, usually
   has no row at all ([model compatibility](reference/model-compat.md#where-the-policy-is-applied)).
3. **Model match** — the model id against the enabled rules: exact matches
   before wildcards, then priority (higher wins), then registration order. A
   wildcard `*` matches any run of characters, including none, and a pattern
   without `*` is exact-only — a plain name never matches as a substring.
   **Implicit routes** are the fallback of this step: when no enabled rule
   matches, the models table is consulted, and a row for the requested model
   routes to that row's provider as a one-entry chain. Rules always win over
   an implicit route. The implicit chain then runs the pipeline below
   unchanged — expansion, credential gate, strategy, capability filter,
   health gate — so it is skipped, filtered or cooled down exactly as an
   explicit chain entry would be
   ([ADR-011](adr/ADR-011-implicit-model-routes.md)).
4. **Chain expansion** — the matched rule's ordered chain. An entry names a
   provider account ID, a `provider-id/model-id` pair, or a provider type. An
   exact account wins; a type entry expands at request time to every
   credentialed account of that type in stable registry order, so a newly
   added account joins the chain without a rule edit. Accounts without a
   credential (an abandoned OAuth draft) are excluded from the expansion and
   the exclusion is traced.
5. **Strategy** — how requests spread across the expanded chain, per rule
   with a global default (`routing_strategy`, default `round_robin`):
   - **Round robin** rotates the chain by one position per request, using a
     per-rule counter, before the capability and health filters — rotation
     reorders candidates and never overrides a gate. The rotation is a trace
     event, including the turn that rotates by zero.
   - **Fill-first** keeps the chain order: the first entry serves everything
     until the health gate opens or a 429 exhausts that provider's retry
     budget, and the next entry then takes over.
6. **Capability filter** — the capabilities the request needs, derived from
   its content, against what the candidate declares. A candidate missing a
   required capability is skipped with a traced reason. Vision is
   deliberately not gated here: static provider flags cannot know which
   *model* accepts images, so the request goes out and a real upstream
   rejection fails over (see the rescue below). The model row's learned
   vision fact is applied last, as a preference among the survivors.
7. **Health gate** — a candidate whose account is in cooldown is skipped with
   a traced reason. If *every* candidate was skipped for cooldown alone, the
   answer is the retryable all-providers-unavailable status rather than a
   404, because the model routed and the accounts are merely pausing
   ([errors](api/errors.md#all-providers-unavailable-retryable)).
8. **Capability fallbacks** — when the whole chain has been attempted and
   every attempt failed with `capability_gap`, the configured vision models
   get one walk of their own in the executor (see the rescue below). Every
   consultation is traced.

Config lives in the database and is edited in the console
([Routing](operating.md#routing)); rule writes reach the live router without
a restart, and only a successfully read rule set replaces the live one — a
failed store read keeps the last known-good rules rather than de-routing
everything. The executor's attempt cap (three chain entries by default) still
applies to the expanded chain, so a very long account pool is not walked in
one request.

## What the filter does with an unknown

A chain entry is filtered on the capabilities the request needs, and the two
kinds of statement behind that decision treat an unknown differently. The
question underneath is always the same: **is a wrong answer worse than a
missing one?**

| Capability | Unknown means | Why |
|---|---|---|
| `streaming`, `tools`, `thinking`, `web_search`, `web_fetch`, `code_execution`, `decision` | not a case — a provider type's defaults, overlaid by the operator's pins, declare the set | these are properties of the account's type and its configuration, decidable before anything is dispatched; a missing flag is a fact, and the entry is skipped with a traced reason |
| `vision` (the model row's learned fact) | **permitted** — only a learned `false` excludes an entry | a row that never formed an opinion is not evidence that the model is blind, and treating it as one would route around every model the sync has not classified |

A candidate is dropped on a declared or learned fact, never on a guess. The
vision gate is the one gate that must also leave a chain standing when it
would otherwise empty it: a chain whose every candidate is known blind is
exactly the case the rescue exists for, and emptying the chain there would
turn a rescue into a hard no-route.

Vision is the case worth watching, because a model that answers with
confidence about an image it never received is the worst outcome in this
system. The guard against it is the learning loop: a real rejection is
recorded on the model row, so the same model is not sent an image twice, and
a chain that has no sighted candidate left is rescued by describing the image
instead (below).

## Capability fallback lists

Three priority lists on the console's **Routing** page put a fallback behind
a capability the routed chain could not serve. They are edited together and
saved in one write, and they are the `vision_fallback`,
`web_search_fallback` and `web_fetch_fallback` settings
([configuration](configuration.md#database-overrides-layer-4)):

| List | Entries | Consumed by |
|---|---|---|
| **Vision fallback** | model IDs (routing identities, e.g. `ollamacloud/glm-5.3`) | the executor's image rescue, after a chain exhausted on `capability_gap` |
| **Web search fallback** | web-tool account IDs | the emulator pool, for emulated `web_search` |
| **Web fetch fallback** | web-tool account IDs | the emulator pool, for emulated `web_fetch` |

Entries are validated against the database at save time — a vision entry must
be an existing model row, a web entry an existing account of a web-tool type
— so a typo fails the save instead of being skipped at request time. A stored
list is re-resolved on every live reload, and an entry that no longer
resolves (a deleted model, a list written by an older release that named
provider accounts in the vision list) is skipped with a log line rather than
taking the rest of the list down with it. An empty or unresolvable web list
keeps the accounts' own insertion order.

## Images on a blind chain: the analyze_image tool

**The rule (ADR-018).** A request whose images matter gets pixels or the
means to look again — second-hand prose alone fails the requirement. What
happens depends on the chain's known vision state:

| Chain vision state | action before dispatch |
|---|---|
| Any entry unknown or can-see | **Pass-through**: the image blocks ride the dispatch natively; no caption, no tool |
| Every entry known blind, vision fallbacks configured | **Pre-dispatch substitution**: every image becomes an `[Bild N (ref …)]` placeholder (bootstrap caption from the cache, else the describer walk) and the `analyze_image` tool is injected; the dispatch carries no image |
| Every entry known blind, no vision fallback | The request goes out as-is; the honest `capability_gap` surfaces and the rescue below answers nothing |

The bootstrap caption is composed from the base prompt only — an image is
described **once** per client, and every later session turn carrying it is a
cache hit; steering a re-description on the client's newest message paid a
describer call per turn and is gone (see ADR-018's 2026-10-18 addendum).
The placeholder embeds a **content ref** — the first 12 hex chars of the
SHA-256 of the image's decoded bytes (the caption cache's own digest), so it
survives the transcript re-send, is identical for an inline payload and a
fetched URL, and costs no storage: the tool can only be called inside the
request that carries the image. An image whose caption failed substitutes
`image content not available` rather than staying raw — on an all-blind
chain the raw image is certain to be rejected, so a spoken absence beats a
doomed round trip (the one deliberate ADR-014 deviation, see
[ADR-018](adr/ADR-018-vision-passthrough-and-analyze-image.md)).

**The tool.** `analyze_image(image_ref, prompt)` is injected executor-side
and answered executor-side: the image plus the model's OWN prompt goes to
the `vision_fallback` models in order (same describer walk as the rescue,
same cache — the tool prompt is the cache's task, so an identical ask is
free and a changed ask re-describes). Bounds mirror the web-tool emulation
(`max_uses` 8, the emulation call/total budgets). The client sees none of
it: no tool definition, no blocks in the response — on every surface,
because the injection happens against the canonical IR after routing, not
in one codec.

**Mixed turns.** A turn that combines an `analyze_image` call with a client
tool call surfaces only the CLIENT's calls; the vision call is dropped and
traced (reason "not answered: mixed with client tool calls") — an unknown
`tool_use` must never reach a client. The ask survives as an `analyze_image`
trace event with the decline.

**Toggles.** `vision_tool=0` degrades to plain pre-dispatch caption
substitution (no tool); `vision_placeholder_template` changes the placeholder
wording (`{n}` `{ref}` `{caption}`); the per-call budget stays
`vision_caption_timeout_ms`
([configuration](configuration.md#database-overrides-layer-4)). Streaming
requests on this path ride the complete loop like web-tool requests — same
documented latency trade.

## The vision rescue

**What triggers it.** After the routed chain has been attempted and *every*
attempt failed with `capability_gap` — an upstream 400 naming the input
modality ("image input is not supported"). This is the REACTIVE half of the
image handling (the pre-dispatch half lives in
[the section above](#images-on-a-blind-chain-the-analyze_image-tool)); on a
chain whose entries are all known-blind it never fires, because the images
never reach the dispatch to be rejected. Nothing else triggers it: a 401 or
a `content_filter` must not be "rescued" by another model, and a chain that
failed for mixed reasons has no rescue to offer. Nor does it rescue a request
that never routed at all: an unresolvable model stays the no-route error. An
empty vision list keeps the original upstream error.

**Describe, do not resend.** The vision list names models that *describe* an
image; they never receive the request. Every image part is extracted — the
walk descends into tool-result content, which can carry images too — and each
one is described by the configured models in order. A describer receives the
image and the instruction: no conversation, no history, no tools. Its context
window is therefore irrelevant to whether it can serve, which is the point —
re-sending the conversation made the rescue unusable in precisely the long
session it exists for. The descriptions then replace the images and the
*same* routed chain is dispatched again, so the model that answers is the one
the operator routed to ([ADR-014](adr/ADR-014-image-to-text-vision-fallback.md)).

Details that matter in operation:

- The instruction is task-aware by default: the client's most recent text
  turn is appended so the description targets the question that came with the
  image, and transcribing visible text is called out. (This task awareness is the reactive
  rescue's alone: the pre-dispatch bootstrap above describes once and stays
  out of the conversation-questions business — those belong to `analyze_image`.) The prompt, the
  task-aware switch and the per-caption timeout are settings
  ([configuration](configuration.md#database-overrides-layer-4)).
- A remote image is downloaded and inlined before it is described, through
  the guarded HTTP client (private ranges and redirects refused). Wire
  encoders differ, and one drops a URL-sourced image outright — which would
  leave the describer commenting on something it never received. If the fetch
  is refused, the image stays in place and the refusal is traced.
- A description that cannot be produced for one image leaves **that image in
  place** — no placeholder, no dropped input. If no image could be described
  at all, the request ends with the original `capability_gap` error rather
  than with a side effect of a rescue that never happened.
- The rescue runs **once**: the re-dispatch carries the fallback disabled, so
  a model that rejects the transcript too ends the request instead of
  describing again.
- Descriptions are cached by image content **and the question that shaped
  them**. The same image with an unchanged question costs nothing on the next
  turn and the substituted text stays stable; a new question re-describes the
  image with that question in the instruction, because the earlier
  description was written to answer the earlier question. A cached
  description replaced the image without an upstream call: it is traced, but
  it produces no sub-operation row, because no operation ran.
- The caption's token spend folds into the request's usage, on the complete
  path and on the streamed one alike.
- Every use is traced: an `image_caption` event per description — the log row
  that shows what the answering model was told the image showed — plus a
  `capability_fallback` event for the consultation and for any image left in
  place.

**Learning.** Every `capability_gap` rejection persists the observed gap into
the model row's capabilities (`"vision": false`), asynchronously and never on
the request path; the row's origin is preserved. A capability the operator
pinned in the model editor ([the console](operating.md#models)) is left alone
— the pin is the operator's statement about that flag, and it stops learning
for exactly that flag until it is removed
([model compatibility](reference/model-compat.md#capabilities-and-pins)).

**The learned gap is read back.** Learning that nothing consumes is just a
write: a row that had learned `vision: false` was still sent the image on
every turn, so an image in a conversation cost one upstream rejection and one
description per request, for the rest of the session. Two consumers close
that loop:

- The router prefers candidates that can see images. With more than one
  candidate surviving the filters, an entry whose row knows it is blind is
  skipped (traced as `capability gap: vision`) as long as at least one
  candidate remains — so a chain with an alternative goes straight to the
  model that can serve the request.
- When every candidate is known blind, the rejection is not a discovery but a
  known outcome, and the executor replaces the images before dispatch —
  which is only done when every image already has a cached description,
  because a partial rewrite would show the model a conversation the client
  never sent.

Both are permissive about rows with no opinion, and neither may empty a chain.

## Transport limits

Every chat upstream rides one guarded transport. Two walls bound a dispatch:
`upstream_response_header_timeout_ms` bounds how long an upstream may accept
the connection and produce **no** response headers (default `300000`, five
minutes), and `vision_caption_timeout_ms` bounds one `vision_fallback`
describe call ([the vision rescue](#the-vision-rescue)) on the same transport.

The five-minute default is deliberate. A request that carries claimed client
web tools or an injected `analyze_image` rides the complete (non-streaming)
emulation loop, so the upstream produces no streamed bytes and the header wall
covers the **whole generation**, not time to first token. Reasoning models
routinely exceed thirty seconds there; the old thirty-second wall turned
healthy answers into transport failures. The default matches the emulation
loop's own total budget (`defaultEmulationTotalBudget`): a re-dispatch still
running at that point ends as the documented 504, not as a provider-health
failure.

A hung upstream becomes a transport-class failure either way — and a
transport-class attempt is not a capability gap, so it **does not trigger the
vision rescue** ([ADR-018](adr/ADR-018-vision-passthrough-and-analyze-image.md)).
A provider that is slow or hangs on multimodal requests therefore needs BOTH
settings raised together: the header wall on the provider type's live set (a
provider write re-applies it; a settings write alone does not rebuild the live
clients), the caption budget on the settings KV. Lower values are also legal:
a faster wall stops a stuck upstream from holding requests before the health
tracker cools it down.

## Context-window rejections fail over

A 400 whose body code (`context_length_exceeded`) or message says the input
exceeds the model's window is its own failure class,
`context_window_exceeded`, and it **fails over** — it is not a client error.
The request is well-formed; only a larger target can serve it. Vendors word
the rejection differently ("prompt is too long", "maximum context length",
"input length exceeds the context length", …), so the matcher reads both the
body code and a collected set of wordings
([errors](api/errors.md#failure-classification)). The class is consulted
after the modality class, so a rejection naming both stays a capability gap —
the modality is what an operator configured a fallback for. Before the class
existed such a 400 was terminal, so a chain died on its first too-small model
instead of reaching a larger one further down
([ADR-013](adr/ADR-013-context-window-aware-fallbacks.md)).

## Non-Claude routing & compatibility

On the Anthropic surface a non-Claude target is a first-class configuration,
governed by one global switch and the per-model policies. The switch,
`anthropic_surface_non_claude`
([configuration](configuration.md#database-overrides-layer-4)), is applied
before compatibility handling and routing, so `block` dispatches nothing: a
model name that is not a Claude-family name is refused with a 400 under
`block`, annotated with a traced `compat_policy` event under `warn`, and
passed untouched under `allow` — the default, also for a missing or invalid
value. How a request that routes is then adapted — thinking handling, server
tool emulation, clamps, context window, cache markers — is
[model compatibility](reference/model-compat.md). The client-side setup is
[Claude Code with non-Claude models](claude-code.md).

## Tool emulation

When a request carries web tools — `web_search`/`web_fetch` server-tool
definitions, or the client's own tools that the `client_web_tools` mapping
claims (Claude Code ships its WebFetch/WebSearch as plain client tools and
would otherwise fetch in the CLI, so the proxy would never see a web-tool
request) — they are converted into client-shaped tools, the model's calls are intercepted
by the executor, answered through the configured web-tool accounts, and
synthesized back into the client's protocol as the family's result blocks
([model compatibility](reference/model-compat.md#the-settings)). The stored
setting keeps the name it was introduced under; the pool it drives is no
longer Firecrawl-only. With no account configured, a call is answered with
the documented `unavailable` error result, so the model can tell the client
it could not look the request up.
That conversion is also what keeps the request routable at all: an emulated
request no longer *requires* native `web_search`/`web_fetch`, so the capability
filter (step 6 above) has nothing to skip and an upstream that has never heard
of the family stays reachable. This is the default policy for every target —
`off` is the only operator pin, and it removes the definitions instead. Vision cannot be emulated this way: a model
that really rejects image input is handled by the rescue above.

Two backend types serve those calls, both connected in the console
(**Providers**):

| Type | Serves | Endpoint |
|---|---|---|
| **Firecrawl** | `web_search`, `web_fetch` | `api.firecrawl.dev/v2` |
| **Ollama Cloud (Web Tools)** | `web_search`, `web_fetch` | `ollama.com/api/web_search`, `/api/web_fetch` — a separate account type from Ollama Cloud's chat endpoint |

They share one ordered walk: the web fallback lists name accounts of either
family, so the two can be mixed and reordered freely, and the amount of
failover is the number of accounts configured — with a single account the
walk is that account, exactly as a lone emulator behaved before the lists
existed. The legacy environment-configured Firecrawl key, where set, joins
both walks as the final entry. Web-tool accounts are never routable as chat
targets — the rule editor refuses a chain entry naming one.

### How an account walk decides

The pool walks its accounts in the configured priority order, and each
attempt has one of three outcomes:

| Outcome | What happens |
|---|---|
| **An answer with results** | The walk stops there. That is the answer, and the substitution names the account that gave it. |
| **An empty answer** (a search with no matches, a page the service could not serve) | Remembered, and the walk CONTINUES — the next account may well have the results. An empty answer still outranks a later account's *failure*: the model is told "nothing found", never "the tool is broken". |
| **A failure** | The walk moves on; the error is surfaced only when no account answered at all. |

A search result of `data.web: []` and a page whose `metadata.statusCode` is
not 2xx are both *empty*, not errors — a backend reports the second one
inside an HTTP 200 body, so the transport succeeding says nothing about
whether there is content. Three bounds keep a slow or broken account from
holding the request:

- **Per-invocation budget** — one tool call, across all accounts, gets 30 s
  (`EmulationCallBudget`). Without it, N accounts would mean N × the client
  timeout inline on the request path. The pool's HTTP client timeout is 20 s,
  deliberately below it, so a stalled account gives up in time for the next
  one to be tried inside the same budget.
- **Per-account circuit** — three consecutive hard failures cool an account
  down (30 s, doubling to a 5-minute ceiling); a success resets it. Cooling
  accounts are skipped, and when *every* account is cooling the list is
  walked anyway as a probe, so a family that had a bad minute recovers by
  itself.
- **Loop budget** — the whole round-trip loop for one request is bounded at
  5 minutes (`EmulationTotalBudget`), and the round count separately at 32.
  On expiry the request ends with what the tools answered so far, the same
  posture the round cap takes.

Every attempt is recorded as an `emulated_tool_attempt` trace event naming
the account and its outcome, and the substitution event names the account
that answered — so "the web tool found nothing" is answerable from the log
instead of being a dead end
([route trace](reference/route-trace.md#sub-operations-come-from-the-trace),
[ADR-015](adr/ADR-015-web-tool-emulation-resilience.md)).

### Backend differences

Ollama's search endpoint takes no domain filters, so a search whose
definition carries `allowed_domains` or `blocked_domains` fails on that
account and the walk moves on to one that can honor the constraint, rather
than returning results that silently ignore it. A fetched page is truncated
to the definition's `max_content_tokens` when that limit is set, which bounds
both what the model is re-sent and what the client receives.

Both backends return a per-result text excerpt (Ollama's `content`,
Firecrawl's `description`). The client wire has no field for it — the
Anthropic `web_search_result` block carries url/title/page_age only — but the
model-facing re-dispatch carries it as `content` on each result. Without it a
search-only client (Codex has no `web_fetch`) receives titles and cannot
answer anything that needs page content; a `web_fetch` client reads the page
instead.

### Streaming requests with emulated tools

A streaming request whose tools include emulated definitions — claimed web
tools, or the injected analyze_image tool of a blind-chain image request —
rides the complete-path emulation round trip to finality, and the final
response is synthesized into a protocol-conform event stream. The latency trade is
explicit: such a request has no token streaming, because the model's whole
answer completes before the first byte reaches the client. That is the price
of real tool results, and it is paid only by requests that carry emulated
definitions; the stream path's failover and capability discipline is
unchanged for every other request.

## Failover semantics

- **Request-level retry** (connection errors, 429, 5xx, honoring
  `Retry-After`) happens only **before the first streamed byte**.
- Once streaming has started, an upstream failure terminates the stream with
  a protocol-conform error event — **there is no mid-stream provider swap**,
  because the client already holds partial output from that provider.
- The terminal chain error and the full route trace are logged; the client
  sees the protocol error, the console shows the transitions. Failover
  transitions, capability-fallback consultations, image descriptions and
  emulated tool calls each get their own sub-operation row, derived from the
  trace by one mapping. Two things are deliberately not rows: a same-provider
  retry, which is a second attempt at the same operation rather than a
  separable one, and a description served from the caption cache, which made
  no upstream call
  ([route trace](reference/route-trace.md#sub-operations-come-from-the-trace)).

## Health tracking

A circuit breaker per account opens after three consecutive failures, backs
off exponentially (5 s doubling to a 5-minute ceiling) and probes for
recovery; cooled-down accounts are skipped with a traced reason, and a
successful request resets the state. A rate-limit open doubles its own
cooldown, because an account that answered 429 was serving other traffic and
will keep doing so for a while.

When every candidate is cooling down, the surfaces answer the retryable
overload signal (529 on Anthropic, 503 on OpenAI). The message names the
actual state — circuit breaker or an upstream quota/rate-limit window — and
`Retry-After` carries the seconds until the earliest provider recovers
([errors](api/errors.md#all-providers-unavailable-retryable)).

**Not every failure is evidence about the provider**, and counting them all
is how one bad request becomes an outage. Opening the breaker is reserved for
the failures a provider is actually responsible for:

| Failure | Opens the breaker | Why |
|---|---|---|
| `capability_gap` | no | the service answered correctly; the model cannot do something |
| `context_window_exceeded` | no | the request did not fit anywhere |
| `content_filter` | no | a property of the content, not of the service |
| client error, other than 401/403 | no | the request or the caller is at fault |
| client error 401/403 | yes | invalid credentials are an account fact |
| `insufficient_quota` | no | it has its own, longer cooldown: the window the upstream announced, or an hourly re-probe |
| client abandoned the request | no | nobody is waiting for the answer |
| a failure that never reached the provider (dial, name lookup, TLS) | no | the network path was broken, not the service; the next chain entry reaches a different host |
| `rate_limit` | yes | the account is under real load |
| `server_error`, and a connection that died after the request went out | yes | what the breaker exists to measure |

Three incidents shaped that table. Three image requests against a text-only
model used to open the breaker for every model on the account — including the
models those requests never touched — because each one produced a capability
gap. A request the client abandoned counted as a failure too, although nobody
was waiting for the answer. And when a client hung up mid-stream, the
dispatch success reported moments earlier *cleared* the breaker the failure
had just opened. The behavior follows the reasoning above: an abandoned
request now reports neither outcome, and a stream that fails after a
successful dispatch corrects that success into a breaker failure, because
the provider really did fail.

An account that has been failing is also reported as **degraded** before it
is taken out of rotation: the last failure before the threshold. A single
threshold otherwise gives a cliff — nothing to see until everything stops
working ([the console](operating.md#quota)).

## Model sync

Providers that expose model discovery mirror their upstream list into the
models table automatically: at startup and on the `model_sync_interval_hours`
cadence (default 24 hours; the feature has its own enable switch). Which
accounts are read is a property of the provider type, not an operator choice:
a custom OpenAI-compatible account is read from its `GET /models`, an Ollama
Cloud account from `GET /v1/models` plus a per-model facts probe
(`POST /api/show`) that supplies the detected facts, and the ChatGPT/Codex
backend from `GET /codex/models?client_version=…`. The Codex manifest is
pinned in `testdata/golden/openai-codex/` (source and access date in its
`.meta.json`); it supplies the context window, reasoning levels, input
modalities and the tool signals the sync maps into model capabilities. An
unexpected answer skips that account with the reason recorded rather than
failing the run.

Policy:

- Upstream models that are new appear as **synced** rows (badged in the
  console).
- Operator edits on an existing row — capabilities, compatibility, priority —
  survive every sync; only the wire name refreshes.
- A sync-authored row that predates a capability fact is repaired once: an
  existing `false` is upgraded to a detected `true`, a missing key stays
  unknown, and any pinned capability or manual row is left untouched.
- Synced rows the upstream dropped are deleted. Rows whose origin is
  `manual` are never touched by detection and never deleted.
- A console save records the row as `manual`, because saving is operator
  authorship. Detection then leaves the row's blobs alone. A manual row whose
  identity the upstream still lists is re-adopted as `synced` on the next run,
  keeping every operator-set field — so `manual` is a statement about who
  last shaped the row, not a permanent exemption from the sync.

The sync also records what the upstream said about each model — capabilities,
context window, and whether the model declares the decision surface — into
the row's blobs, filling only what the operator left empty and never
overwriting a pin. The row's capability blob — detected, pinned or learned —
is what the vision gate reads; the decision flag documents the model and does
not gate decision routing, which is an account capability
([model compatibility](reference/model-compat.md#capabilities-and-pins)).

Per-run outcomes (added, updated, removed and skipped per account, with the
reason) land in the `model_sync_state` setting and on the console's Models
page. A dead upstream skips loudly and never fails the run
([configuration](configuration.md#database-overrides-layer-4),
[ADR-010](adr/ADR-010-model-sync-and-capability-fallbacks.md)).
