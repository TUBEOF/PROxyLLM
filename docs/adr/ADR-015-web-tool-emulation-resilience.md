# ADR-015: Web-tool emulation resilience

- **Status**: Accepted
- **Date**: 2026-10-10

## Context

The web-tool emulation layer (ADR-010) gives `web_search`/`web_fetch` the same
failover discipline the chat chains have: an ordered list of Firecrawl accounts,
walked until one answers. In the field the operator reported three symptoms —
searches that sometimes returned nothing, fetches that came back wrong, and
calls that took an unbounded time — and reading the code showed four distinct
causes, none of which was Firecrawl being slow:

1. **An empty answer was indistinguishable from a failure.** The walk stopped on
   the first account that returned no error, and an empty result set is not an
   error — so a search finding nothing in account A never reached account B,
   which may well have had the results. The reference makes this exact shape
   explicit: `data.web: []` with `success: true`.
2. **A page the service could not serve looked like a document.** Firecrawl
   reports an unservable page INSIDE an HTTP 200 body, via `metadata.statusCode`
   and `metadata.error`. The adapter read `data.markdown` and handed the model
   the 404 page's own text as if it were the page it asked for.
3. **`warning` was typed as an array.** The reference types it as a nullable
   string. A non-empty warning therefore failed `json.Unmarshal` for the whole
   response, turning a soft "1 of 3 sources unavailable" note into a hard
   emulator error.
4. **Nothing was bounded.** The walk had no budget of its own, so one tool call
   could cost N × the client timeout inline; the round-trip loop was bounded in
   COUNT (32 rounds) but not in time; and a misconfigured account was retried
   first on every call, paying its failure every time.

A fifth problem was diagnostic rather than functional: the walk happened inside
the pool, so the log could say a web tool ran but not which account was asked or
why it found nothing.

## Criteria

- **An answer must be an answer.** "Nothing found" is information the model can
  act on; it must never be dressed up as an error, and an error must never be
  dressed up as a document.
- **Bounded latency on the request path**, whatever the account count.
- **Attribution**: the log explains a web tool's outcome, including "nothing".

## Options considered

| Option | Strengths | Weaknesses |
|---|---|---|
| A sentinel error for "empty" | No walk change | Conflates "answered nothing" with "failed" at every call site, and the model would be told the tool broke |
| A richer result type (`Empty bool`) on the Emulator interface | Explicit | Touches every implementation and every test fake for no capability the pool closure does not already have |
| Per-account circuit keyed by list position | No wiring change | Forgets a cooling account the moment the operator reorders the list |
| Restore token streaming for the emulated class | Familiar shape | Puts tool round trips on the client wire and breaks ADR-005's no-mid-stream-swap invariant |
| Keep the emulated class buffered | Preserves ADR-005 | The client waits for the whole answer (documented; a streaming option exists) |

## Decision

**A web-tool account walk has three outcomes — answered, empty, failed — empty
outranks failure, and every attempt is bounded and attributed.**

Concretely:

- `walkPool` returns on a non-empty answer, continues past an empty one, and
  surfaces the last error only when nothing answered at all. Emptiness is
  reported by the pool's own closure, so the `Emulator` interface stays two
  methods.
- One `EmulationCallBudget` (30 s) per invocation across all accounts; the
  shared emulator HTTP client uses a 20 s timeout, deliberately below it, so a
  stalled account leaves room for the next.
- One `EmulationTotalBudget` (5 min) for the whole round-trip loop, applied only
  once the request IS an emulation loop; on expiry the request ends with what
  the tools answered, the same posture the round cap takes.
- A per-account circuit: three consecutive hard failures cool an account down
  (30 s, doubling to a 5-minute ceiling), a success resets it, and when every
  account is cooling the list is walked anyway as a probe. Accounts are
  identified by provider row id, which survives a live swap.
- Each attempt is an `emulated_tool_attempt` event (route_trace v7, additive)
  naming the account and its outcome, and the substitution event names the
  account that answered.
- Every account shares one HTTP client, so connections are reused across a
  failover.
- The emulated class stays on the complete path (ADR-005), unchanged.

## Consequences

Easier: a search that finds nothing in one account now tries the next, and a
misconfigured account stops taxing every request after three failures. A web
tool that returns nothing is explainable from the log.

Harder: failover can now cost a walk PAST accounts that answered empty, which is
more upstream work than before — bounded by the call budget, and paid only when
the alternative is returning nothing. The circuit adds per-account state that a
live swap must key correctly (hence row ids, not positions).

Accepted trade-offs: no response cache (caching search results shares content
between clients, which is an explicit operator decision, not a default); a
transient error still costs the walk one account (an inline retry was rejected
because it multiplies latency inside a fixed budget); token streaming for the
emulated class is deferred to a change that would amend ADR-005.

## Addendum 2026-10-11: a second backend, a bounded page, and a page that failed

The operator reported `web_fetch` still failing after this ADR landed: Claude Code
showed the fetch returning 1.4 MB, then the turn came back with no assistant text.
Reading the code found that the fetch's **encode** path had never been tested —
every fetch test stopped at the IR part, and the one encode test round-tripped
through the same codec, so encoder and decoder agreed with each other by
construction and a wire defect was invisible.

Three corrections, plus the second backend the operator asked for:

1. **`max_content_tokens` was decoded and never enforced.** The parameter is
   documented behaviour, not a hint — "If the fetched content exceeds this limit,
   the tool truncates it" — and it is what bounds the re-dispatch message. A
   multi-megabyte page rode into the upstream conversation at full size, and the
   upstream answered with an empty turn, which is the failure the operator saw.
   The budget is now applied at the single point where a page is accepted, so it
   covers both the model-facing payload and the result block the client receives.
   Optional: zero still means no limit, and it never applies to binary content.
2. **An unservable page is now the documented error.** Cause 2 above made such a
   page *empty*; empty then encoded as a success whose document block carried no
   `source.data` — a shape the reference never produces. It maps to
   `url_not_accessible` ("Failed to fetch content (HTTP error)"). The pool's rule
   that an empty answer outranks a later failure is untouched: that decides which
   ACCOUNT serves the call, not what the client is told.
3. **A `web_fetch` result with no payload no longer falls through to the search
   branch**, which answered a `web_fetch` use with a `web_search`-shaped content
   array.

**The pool was left backend-agnostic, and took the second backend without an
interface change.** An entry is an id plus the two-method `Emulator`; there is no
backend-type field, which is why `internal/tools/ollamaweb` (Ollama's hosted
`/api/web_search` and `/api/web_fetch`) needed only a catalog entry, a config
parse case and a `buildProvider` case to join the same ordered walk. It is a
separate account type from `ollama-cloud`, not a second role for the chat
account: a provider row is either an LLM endpoint or a web-tool backend, and the
two roles stay independently orderable and independently revocable.

Two limits of that backend are surfaced rather than papered over. Its search
endpoint takes no domain filters, so a search whose definition carries them
**fails** — letting the walk try a backend that can honour the constraint —
instead of silently returning unfiltered results. And its per-result `content`
snippet is dropped, because neither `ir.WebSearchResultItem` nor the Anthropic
`web_search_result` block has a plain-text field to carry it; adding one is a
wire change, not a backend concern.

## Addendum 2026-10-17: emulation is the default for every target

The resilience decisions above assumed the emulation policy would be reached.
It was not, in two shapes found in production logs: the policy was resolved
from the client-typed model name, so a Claude Code auxiliary ID resolved through
a `claude-*` alias (and any model without a row of its own) fell back to the
zero policy — emulation off — and the stored default itself was `native`
whenever no web-tool account existed. Either way the dated server-tool definition
rode to an upstream that has never heard of it, the capability filter skipped
every account that does not declare native web search, and the request died as a
non-retryable 404 **before** any of this ADR's machinery — the account walk, the
circuit, the `unavailable` degrade — could run. A request that needs a web tool
must never be unroutable because of one.

Two decisions follow. **Emulation is the policy for every target**: `emulate-firecrawl`
unless the row explicitly pinned `off`, and a stored legacy `native` reads as
emulation — no provider type in this proxy speaks the server-tool wire, so
`native` could only ever produce the broken shapes above. The account set decides
the ANSWER, not the policy: with accounts the walk serves results, without any
the call gets the documented `unavailable` error result. And **the policy is
resolved for the model the request lands on** — the same one-hop alias
resolution the router routes with, shared through `Router.AliasTarget`, so the
policy and the routed model cannot name different models.
