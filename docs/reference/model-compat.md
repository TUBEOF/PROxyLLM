# Model compatibility

A model row carries more than an identity: it carries the policy that
decides how a request for it is adapted before dispatch. This page is the
single home for those settings; other pages link here instead of restating
them.

Everything on this page is edited in the console's **Models** page and
applied per request. Every application is recorded in the request's
`route_trace` — an adaptation is never silent.

Since [ADR-018](../adr/ADR-018-vision-passthrough-and-analyze-image.md) a
learned or pinned `vision: false` does more than reorder candidates: on a
chain whose entries are all known blind, images are substituted pre-dispatch
and the `analyze_image` tool is injected, so the model can interrogate the
image with its own prompts.

## Capabilities and pins

A row carries a capability blob with eight flags: `streaming`, `tools`,
`vision`, `thinking`, `decision`, `web_search`, `web_fetch`,
`code_execution`.

- **Detected.** For a synced row the model sync fills the blob from the
  upstream's own metadata (see [routing and
  fallbacks](../routing-and-fallbacks.md#model-sync)). A later sync repairs an
  existing `false` in a sync-authored row to a newly detected `true`; it never
  invents a missing flag and never touches a pin. `decision` is one of
  those facts: it says whether the *model* declares the decision surface —
  it does not gate decision routing, which is an account capability
  ([decision API](../api/decisions.md#routing)).
- **Pinned.** Saving a row in the console writes every flag *and* a
  `pinned` map naming the flags the operator set. A pin stops the automatic
  learning for exactly that flag; unpinning it lets learning resume.
- **Learned.** When an upstream rejects a request because it cannot take an
  input modality, the proxy records that as a capability fact on the row —
  a `vision: false` learned from a real rejection is what keeps the next
  turn from re-sending the image. Learning never touches a pinned flag and
  never flips a row's origin.

The distinction between *unknown* and *false* matters: a row that says
nothing about `vision` is treated as unclassified (and is permitted), while
an explicit `false` is a statement that the model cannot see images.

## The settings

| Setting | Values | Effect |
|---|---|---|
| `thinking_handling` | `strip` (default off-Anthropic), `map-to-reasoning-effort`, `block` | How the `thinking` parameter and thinking blocks in history are handled, and (see below) what happens to thinking an upstream volunteers |
| `server_tool_emulation` | `emulate-firecrawl` (default for every target), `native`, `off` | Whether the request's web tools — dated server-tool definitions (`web_search_*`, `web_fetch_*`) and the client's own tools the `client_web_tools` mapping claims (Claude Code's `WebFetch`/`WebSearch`) — are converted into emulated tools served by a web-tool account, ride to the upstream, or are removed. `off` is the only reachable pin: the derived default is emulation, and a stored legacy `native` reads as emulation (no provider type in this proxy speaks the server-tool wire) |
| `max_tokens_clamp` | integer, `0` = off | Caps `max_tokens` for a target with a lower output limit. Operator-set only — the proxy never guesses a target's limit |
| `context_window` | integer tokens, `0` = unknown | Feeds the capability filter and the token estimate. Detected from upstream metadata, overridable by the operator |
| `cache_control` | `forward` (Anthropic→Anthropic only), anything else = drop | Whether `cache_control` markers survive translation |

Two of these have a second, subtler half:

**Thinking is governed in both directions.** Whether the client asked for
thinking is captured *before* any rewrite, so a stripped parameter does not
erase the intent. If the client did not enable thinking, thinking blocks
from the response are dropped — complete and streamed alike, with streamed
block indexes renumbered so the client never sees a gap. The reason is
concrete: a client that reads the first content block as the answer would
otherwise see an empty reply. The trace records this as a `compat_policy`
event with `thinking_handling=strip-output`.

**Emulation is about the target, not the request.** A dated server tool
cannot ride to a provider that has never heard of it, so the definition is
converted into a client-shaped tool with the family's own input schema
(`query` for search, `url` for fetch), the model's call is intercepted by the
executor, answered through the configured web-tool accounts, and the result is
synthesized back into the client's protocol. The model rows themselves always
record `web_search`/`web_fetch`/`code_execution` as `false`: those families are
served by accounts and fallback lists, not by individual models.

A client's own web tool is claimed the same way when the surface maps its name
to a family (`client_web_tools`, Anthropic surface only — the one wire that can
carry the family's result blocks): Claude Code sends `WebFetch`/`WebSearch` as
plain client tools and would otherwise fetch in the CLI itself, so the proxy
would see no web-tool request at all. Claiming one rewrites the definition into
the family def; the client stops doing the work, the proxy serves it, and the
call appears in the log like any other emulated call. A forced `tool_choice`
follows the rename (and degrades to `auto` when `off` removes the definition),
so the upstream never receives a dangling name. A turn that mixes an emulated
call with the client's own calls is still answered by the proxy, and the round
cap/budget synthesize an `unavailable` result: an emulated family name is never
returned as a client `tool_use`.

Emulation is therefore the default for **every** target, not a property a
web-tool account has to be present for. Two things go wrong when a definition
rides native instead: the router's capability filter skips every account that
does not declare native support (no chat account type does), which turns the
request into a no-route 404 before any fallback can run; and a model that calls
the definition produces a tool call the client cannot answer. What varies with
the account set is only the ANSWER: with web-tool accounts configured the walk
serves real results (search then fetch over the ordered fallback list), and with
none configured every call gets the documented `unavailable` error result, so
the request still routes and the model can tell the client it could not look the
request up.

## Defaults per origin

| Row origin | Capabilities | Compat |
|---|---|---|
| `synced` | Detected facts from the upstream, plus explicit `false` for what it did not report | Detected context window; everything else derived |
| `manual` | Whatever the operator saved | Whatever the operator saved |

A request whose model has no row of its own — every alias target the alias map
names, and any hand-typed name the sync has not imported — takes the derived
defaults above. The operator's policy belongs on the row the request lands on,
which is why the lookup follows the alias hop (next section) before it reads.

A save in the console turns a synced row into a `manual` one. That keeps
the sync's hands off it only while its ID is not one the upstream reports:
a manual row whose ID *is* an upstream model ID is re-adopted as `synced`
on the next run (operator edits survive; the row becomes tracked again).
An ID the upstream never offers — a hand-written alias-style ID — stays
manual permanently.

## Legacy compat blobs

The compat knobs used to be edited directly. Rows that still carry such a
blob keep working as read-only overrides: the stored value wins where it is
set, and the derived policy fills the rest. Newly saved rows do not send a
compat blob at all, so there is nothing to drift.

## Where the policy is applied

- **Request path**: the surface handler resolves the policy of the model the
  request will actually route as (`compatFor`): the Claude Code context marker is
  stripped and the alias map is followed one hop first, so a client-typed ID such
  as `claude-haiku-5-5` picks up its alias target's row instead of silently
  dropping to the defaults. That resolution is the same one the router performs,
  shared through `Router.AliasTarget` — the policy and the routed model can never
  name different models.
- **Routing**: `max_tokens_clamp` and `context_window` participate in the
  capability filter; `server_tool_emulation` decides whether the emulation
  loop runs at all.
- **Response path**: the unsolicited-thinking filter and the streamed
  variant of it.

The field-level effects on each protocol are in the
[translation matrix](translation-matrix.md).
