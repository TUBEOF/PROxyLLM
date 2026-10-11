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
| Keep the emulated class buffered | Preserves ADR-005 | The client waits for the whole answer (documented; a phase-2 option exists) |

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
