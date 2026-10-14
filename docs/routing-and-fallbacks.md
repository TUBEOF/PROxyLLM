# Routing & Fallbacks

The routing engine is deterministic and observable: every
skip, retry, failover, compat-policy application, tool-emulation
substitution, chain expansion, and strategy rotation lands in the request's
`route_trace` — **fallbacks are never silent**.

## Model identity
A model's routing identity is `<prefix>/<wire-name>` (e.g.
`ollamacloud/glm-5.3`): the prefix is a per-provider-type setting
(editable on the provider type's page, default = the type string), the
wire name is what the upstream expects. The models table stores both
(`ID` = routing identity, `Name` = wire name); the model sync creates
prefixed rows, and legacy unprefixed rows keep working (wire name = ID).
Before dispatch, the resolved chain's routing ID maps to the wire name
(row wins; a missing row falls back to the tail after the last slash) —
traced as `model_resolved` (route_trace v4). The client-facing response
echoes the routing identity, never the wire name. Aliases
(`model_aliases`) map client-facing names to routing identities and match
in the rule step below.

## Rule model (evaluation order)

1. **Key scope** — the client key's allowed model patterns.
2. **Model match** — exact model id, wildcard, or alias (the
   `model_aliases` setting maps Claude Code naming to your models).
   **Implicit routes (ADR-011):** when no enabled rule matches, the
   models table is consulted — a row for the requested model ID routes
   to that row's provider as a one-entry chain, traced as
   `implicit_route`. Rules always win (any matching rule suppresses the
   implicit route); key scopes and aliases resolve first as above. A
   dead, uncredentialed, or cooled-down provider still yields the honest
   `chain empty …` rejection with skip events — same behavior as an
   explicit rule. To take a model out of service without a rule, delete
   or disable its model row (or use key scopes).
3. **Chain resolution** — the ordered fallback chain for the matched
   rule. An entry names a provider ID, a `provider-id/model-id` pair, or
   a provider TYPE (ADR-009): a type entry expands to all accounts of
   that type at request time, so a newly added account joins the
   rotation without any rule edit. Accounts without a credential (an
   abandoned OAuth draft) stay out of the expansion.
4. **Strategy** (ADR-009) — how requests spread across the expanded
   chain. Per rule, with a global default (`routing_strategy` setting,
   default `round_robin`):
   - **Round Robin** — each request rotates the chain by one position;
     the rotation is a trace event.
   - **Fill-First** — the chain order is kept: the first entry serves
     everything until the health gate opens (breaker cooldown) or a 429
     exhausts its retry budget, then the next entry takes over.
5. **Capability filter** — deterministic surface/emulation capabilities
   derived from the request content (web_search, web_fetch,
   code_execution, streaming, context size vs. window); providers failing
   the filter are skipped **with a logged reason**. Vision is deliberately
   NOT gated here: static provider
   flags cannot know which MODEL accepts images — the request goes out,
   and a real upstream rejection fails over.
6. **Health gate** — circuit breaker (consecutive failures), exponential
   backoff, probe recovery; cooled-down providers are skipped or
   deprioritized.
7. **Capability fallbacks** (ADR-010, amended 2026-09-21) — when the whole
   chain failed with `capability_gap` rejections (a 400 naming the input
   modality, e.g. "image input is not supported"), the global
   `vision_fallback` MODEL list (Routing page) gets one walk of its own in
   the executor — every consultation is a traced `capability_fallback`
   event. An empty list keeps the documented upstream error.

Config lives in the DB and is editable in the WebUI (**Routing**); rule
writes reach the live router without restart — only a successfully read
rule set swaps in (a failed store write keeps the last known-good rules).
The executor's attempt cap (2–3 providers per request) still applies to
the expanded chain — a very long account pool is not walked in one
request.

## When a capability is unknown

A chain entry is filtered on the capabilities the request needs, and each of
those decisions answers the same question differently: **is a wrong answer
worse than a missing one?** A request is never silently sent to a target that
cannot serve it, but "we do not know" is not one value.

| Capability | Unknown means | Why |
|---|---|---|
| `streaming`, `tools`, `thinking`, `web_search`, `web_fetch`, `code_execution` | **permitted** — only a declared `false` excludes an entry | a target that turns out not to support the flag produces a worse answer, not a false one; the answer itself is still about the request |
| `vision` | **permitted** — and the image is handled by the rescue below | the rescue exists precisely for this: a model that cannot see images answers a 400, the images are described, and the chain is retried. An unnecessarily described image costs fidelity; a wasted round trip costs time |

The one thing an unknown capability must never do is make the request fail: a
chain whose every entry is filtered out is a hard no-route, so the filter only
ever narrows a chain that still has a member.

Vision is the case worth watching, because a model that answers with confidence
about an image it never received is the worst outcome in this system. If a model
row that has no opinion about vision is ever seen doing that, the rule above is
the one to revisit — the model table can carry the fact, and the router already
reads it.

## Capability fallback lists (ADR-010, amended 2026-09-21)

The Routing page holds three global priority lists (drag-drop ordered,
saved in one write):

| List | Entries | Semantics |
|---|---|---|
| **Vision fallback** | MODEL IDs (routing identities, e.g. `ollamacloud/glm-5.3`) | After a chain exhausted on `capability_gap` rejections, these models DESCRIBE each image (they never receive the conversation); the descriptions replace the images and the routed chain is dispatched again (ADR-014) |
| **Web search fallback** | Web-tool account IDs (firecrawl, ollama-cloud-web) | The emulator pool tries accounts in order on emulated `web_search`; a failing account fails over to the next |
| **Web fetch fallback** | Web-tool account IDs | Same as above for `web_fetch` |

**Spec change 2026-09-21** (breaking for the vision list): entries are
MODEL IDs validated against the models table at save time — provider
IDs/types from v1.0.2 configurations no longer resolve and are skipped
with a log line until re-saved. The fallback moved from the router's
pre-stream gate to the executor: it fires only after the routed models
REALLY rejected the request at the upstream (HTTP 400 naming the input
modality — image, multimodal, unsupported content), never to rescue an
unresolvable chain (that stays the no-route error).

**Image-to-text fallback (ADR-014, 2026-10-10 — replaces the whole-conversation
resend)**: the vision list names the models that **describe** an image; they
never receive the request. When a chain exhausts on `capability_gap`
rejections, the executor finds every image part (descending into tool-result
content, which may carry images too), asks the configured vision models in
order to describe each one — a model gets the image and the instruction, no
history, no tools — and substitutes each description for its image. The routed
chain is then dispatched again, so the model that answers is the one the
operator routed to. Because the describer only ever sees an image, **its
context window is irrelevant**, which is the point: the rescue used to
re-send the conversation and was therefore unusable precisely in the long
session it exists for.

Details that matter in operation:

- The instruction is task-aware by default: the client's most recent text turn
  is appended (settings `vision_caption_prompt`, `vision_caption_task_aware`,
  `vision_caption_timeout_ms`), and visible-text transcription is called out.
- A remote image (URL source) is downloaded and inlined first, through the
  guarded HTTP client. Wire encoders differ, and one drops a URL-sourced image
  outright — which would leave the describer commenting on something it never
  received. If the fetch is refused, the image stays in place and the refusal is
  traced.
- A description that cannot be produced for one image leaves **that image in
  place** — no placeholder, no dropped input. If no image could be described at
  all, the request ends with the ORIGINAL `capability_gap` error.
- The rescue runs **once**: the re-dispatch has the fallback disabled, so a model
  that rejects the transcript too ends the request instead of describing again.
- Every use is traced (`image_caption`, plus `capability_fallback` for the
  consultation and for any image left in place), and `image_caption` is its own
  log row: the operator can see what the answering model was told the image
  showed. Caption token spend is folded into the request's usage.

**Context-window rejections fail over (ADR-013, 2026-10-09)**: a 400 whose
body code (`context_length_exceeded`) or message says the input exceeds the
model's window is its own class, `context_window_exceeded`, and **fails
over** — it is not a client error. The request is well-formed; only a
larger target can serve it. Vendors word this differently ("prompt is too
long", "maximum context length", "input length exceeds the context length",
…), so the matcher consults the body code and the message. Before this
class existed such a 400 was terminal, which meant a chain died on its first
too-small model instead of reaching a larger one further down.

**Wiring note**: the two web-tool lists are now CONSUMED —
until this fix the pool was assembled in registry account order and the
stored lists were validated but ignored. Pool assembly resolves each
entry's account to its emulator and walks the lists in priority order;
unknown accounts (e.g. a deleted account named in a stale list) are
skipped with a log line, and an empty list keeps the insertion order.

**Streaming emulation**: a STREAMING request whose tools
include emulated defs rides the complete-path emulation round trip to
finality, and the final response is synthesized into a protocol-conform
event stream. Latency trade: such a request has no token streaming (the
model's whole answer completes before the first byte) — the trade for
real tool results; relaying raw tool_use (the previous behavior) was
neither conform nor useful. The stream path's failover/capability
discipline is unchanged for requests WITHOUT emulated defs.

**Automatic capability learning**: every `capability_gap` rejection
persists the observed gap into the model row's capabilities (e.g.
`"vision": false`) — asynchronously, never on the request path. A pinned
flag (the operator's manual selection in the Models editor, written as
`"pinned": {"vision": true}`) stops the learning for exactly that
capability; the row's sync origin is preserved. The editor composes that
map and the server stores the blob verbatim: checking a capability pins
it, unchecking it removes the pin, and the learning then resumes for that
capability alone.

**The learned vision gap is read back** (spec change 2026-10-11). Learning
that nothing consumes is just a write: a model row that had learned
`vision:false` was still sent the image on every turn, so an image in a
conversation cost one upstream rejection — and one image description — per
request, for the rest of the session. Resolution now drops a candidate
whose row *knows* it cannot see images when the request carries one, so a
chain with an alternative goes straight to the model that can serve it.
Two limits keep this honest: a row with **no opinion** is permissive (most
rows are unclassified, and treating "unknown" as "blind" would route around
them all), and the gate **never empties a chain** — every candidate being
known-blind is exactly the case the description walk exists for, and that
request still has to go out and be rejected for the rescue to fire.

The web lists refine the executor's emulator pool: with exactly one
account the behavior is identical to the pre-ADR-010 single emulator; with
several, a dead backend fails over (the executor's emulation loop is
unchanged — the pool is just what it calls). Web-tool accounts are never
LLM-chain-routable; the rule editor refuses them.

## Failover semantics

- **Request-level retry** (connection errors, 429, 5xx — honoring
  `Retry-After`) only **before the first streamed byte**.
- Once streaming, an upstream failure terminates the stream with a
  protocol-conform error event — **no mid-stream provider swap**.
- The terminal chain error plus the full route trace is logged; the
  client sees the protocol error, the WebUI shows the transitions.

### Fallbacks as their own log rows
Fallback transitions are never silent, and since this release the ones an
operator most needs to see are no longer nested only inside the parent
request's `route_trace` — they are their own rows in the WebUI log:

- a **chain failover** (the request moving to the next chain entry),
- a **capability fallback** consultation and the fallback model it walks
  to (the vision fallback — ADR-010),
- an **emulated web-tool** call (`web_search`/`web_fetch` served by the
  Firecrawl fallback, §6.2),
- a **same-provider retry** after backoff.

In the log these hang under the request that caused them (nested rows in
the flat table, with a `↳ N ops` chip on the parent, and a provider/model
chain summary in the request's detail drawer). They are derived from the
trace by a single mapping, so what the log shows and what `route_trace`
records cannot drift apart.

## Health tracking

A circuit breaker per provider opens after consecutive failures, backs
off exponentially, and probes for recovery. Cooled-down providers are
skipped with a route_trace reason; recovery is recorded too.

**Not every failure is evidence about the provider**, and counting them all is
how one bad request becomes an outage. Opening the breaker is reserved for the
failures a provider is actually responsible for:

| Failure | Opens the breaker | Why |
|---|---|---|
| `capability_gap` | no | the service answered correctly; the model cannot do something |
| `context_window_exceeded` | no | the request did not fit anywhere |
| `content_filter` | no | a property of the content, not of the service |
| client error, other than 401/403 | no | the request or the caller is at fault |
| client error 401/403 | yes | invalid credentials are an account fact |
| `insufficient_quota` | no | it has its own, longer cooldown: the window the upstream announced |
| client abandoned the request | no | nobody is waiting for the answer |
| `rate_limit` | yes | the account is under real load |
| `server_error`, transport failure | yes | what the breaker exists to measure |

Three image requests against a text-only model used to open the breaker for every
model on that account, because each one produced a capability gap. A request the
client abandoned counted as well — and then, when the client hung up mid-stream,
the dispatch success reported afterwards *cleared* the breaker instead.

An account that has been failing is also reported as **degraded** before it is
taken out of rotation: the last failure before the threshold. A single threshold
otherwise gives a cliff — nothing to see until everything stops working.

## Non-Claude routing & compatibility

On the Anthropic surface, non-Claude models are first-class: the global
`anthropic_surface_non_claude` switch (`allow`/`warn`/`block`) plus
per-model compat settings (`thinking_handling`, `server_tool_emulation`,
`max_tokens_clamp`, `context_window`, `cache_control`). See
[Claude Code with non-Claude models](claude-code-with-non-claude-models.md)
and the [translation matrix](reference/translation-matrix.md).

## Tool emulation

When a request needs `web_search`/`web_fetch` and the routed provider
lacks native support, the emulation layer substitutes
(protocol-conform result blocks) when the model's `server_tool_emulation`
is `emulate-firecrawl` — the substitution is recorded in route_trace and
the response metadata. The stored value keeps the name it was introduced
under; the pool it drives is no longer Firecrawl-only.

Two web-tool backends are available, both connected in the WebUI
(Providers, ADR-010):

| Type | Serves | Endpoints |
|---|---|---|
| **Firecrawl** | `web_search`, `web_fetch` | `api.firecrawl.dev/v2` |
| **Ollama Cloud (Web Tools)** | `web_search`, `web_fetch` | `ollama.com/api` — a separate account type from Ollama Cloud's chat endpoint |

They share one ordered walk: the web fallback lists name accounts of
either family and the pool tries them in that order, so the two can be
mixed and reordered freely. `PROXYLLM_FIRECRAWL_API_KEY` ENV
remains the fallback when no account exists, and with neither, emulation
returns the documented "unavailable" error results. Multiple accounts +
the web fallback lists give the emulation real failover. Note one
difference between the families: Ollama's search endpoint takes no domain
filters, so a search whose definition carries `allowed_domains` /
`blocked_domains` fails on that account and fails over — rather than
returning results that ignore the constraint. A fetched page is truncated
to the definition's `max_content_tokens` when that limit is set.

Vision cannot be
emulated: a model that really rejects image input at the upstream fails
over within the chain, then to the configured vision fallback MODELS —
see above.

### How a web-tool account walk decides
The pool walks its accounts in the configured priority order, and each
attempt has one of three outcomes:

| Outcome | What happens |
|---|---|
| **An answer with results** | The walk stops there. That is the answer. |
| **An empty answer** (a search with no matches, a page the service could not serve) | Remembered, and the walk CONTINUES — the next account may well have the results. An empty answer still outranks a later account's *failure*: the model is told "nothing found", never "the tool is broken". |
| **A failure** | The walk moves on; the error is surfaced only when no account answered at all. |

A search result of `data.web: []` and a page whose `metadata.statusCode` is
not 2xx are both *empty*, not errors — the reference reports the second one
inside an HTTP 200 body, so the transport succeeding says nothing about
whether there is content.

Three bounds keep a slow or broken account from holding the request:

- **Per-invocation budget** — one tool call (across all accounts) gets
  `EmulationCallBudget`, default 30 s. Without it, N accounts meant N × the
  client timeout inline on the request path. The pool's HTTP client uses a
  20 s timeout, deliberately below it, so a stalled account gives up in time
  for the next one to be tried inside the same budget.
- **Per-account circuit** — three consecutive hard failures cool an account
  down (30 s, doubling to a 5-minute ceiling); a success resets it. Cooling
  accounts are skipped, and when EVERY account is cooling the list is walked
  anyway as a probe, so a family that had a bad minute recovers by itself.
- **Loop budget** — `EmulationTotalBudget`, default 5 min, bounds the whole
  round-trip loop (the round count is capped separately at 32). On expiry the
  request ends with what the tools answered so far, the same posture the round
  cap takes.

Every attempt is recorded in `route_trace` as an `emulated_tool_attempt`
event (route_trace v7) naming the account and its outcome, and the
substitution event names the account that answered — so "the web tool found
nothing" is answerable from the log instead of being a dead end.

## Model sync (ADR-010)

Providers exposing model discovery (custom OpenAI-compatible `GET
/v1/models`, Ollama Cloud `GET /api/tags`, codex backend best-effort)
mirror their upstream list automatically: at startup and every
`model_sync_interval_hours` (default 24). Policy:

- New upstream models appear as **synced** rows (badged in the WebUI).
- Operator edits (capabilities, compat, priority) on existing rows
  survive every sync — only names refresh.
- Synced rows the upstream dropped are deleted; **manual rows are never
  touched** by the sync.
- Sync saves from the WebUI convert a row to manual (the sync stops
  tracking it).

Per-run outcomes (added/updated/removed/skipped per provider) land in the
`model_sync_state` setting and on the Models page; a dead upstream skips
loudly and never fails the run.