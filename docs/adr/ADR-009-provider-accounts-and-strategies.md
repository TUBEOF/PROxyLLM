# ADR-009: Provider accounts and routing strategies

- **Status**: Accepted
- **Date**: 2026-09-16
- **Amends**: ADR-005 (routing rule model — determinism scope), ADR-006 (route_trace schema versioning)

## Context

The WebUI provider page is alpha-state: it edits a raw JSON config blob in a
textarea, cannot represent multiple accounts per provider type (several
Ollama Cloud API keys, several Codex OAuth logins), and the routing engine
has no strategy concept — the chain order is the whole story. Users expect
a pooled-account experience: provider cards, pooled accounts per
provider, and strategies (round robin, fill-first) that spread requests
over all accounts automatically, with automatic inclusion of newly added
accounts and fallback when an account is exhausted.

Today one `providers` row carries exactly one credential
(`provider_tokens (provider_id, kind)`), routing chains reference provider
IDs, and the entire stack (tokens, models FK, health tracker, registry,
usage) is keyed on provider ID. Upstream providers expose no quota APIs,
so "100% used" can never be observed directly.

## Criteria

- **P0** Multiple accounts per provider type, added via forms or a guided
  OAuth flow — no JSON editing anywhere in the UI.
- **P0** ADR-005 invariants hold: failover only before the first streamed
  byte; every transition recorded in route_trace; never a silent default.
- **P0** No secret material in responses, logs, or config columns
  (ADR-007 envelope + redactor doctrine).
- **P1** New provider types plug in with one catalog entry, one config
  parse case, one construction case.
- **P1** Minimal migration surface — existing single-account databases
  keep working unchanged.

## Options considered

| Option | Strengths | Weaknesses |
|---|---|---|
| New `accounts` table FK to providers | Explicit account model | Rewires tokens, chains, executor, health — every layer is already keyed on provider ID; large blast radius for zero routing benefit |
| N `providers` rows per type (chosen) | Every layer already supports it; accounts are providers; chains, health, usage work unchanged | "Account" is implicit — needs the catalog + per-type grouping in the UI to make the model legible |
| Quota-based fill-first (upstream quota APIs) | Predictable distribution | Upstream quota APIs do not exist (OpenAI/ChatGPT usage limits are not queryable) — the strategy would be fiction |
| Health-gated fill-first (chosen) | Uses the real exhaustion signals the proxy already has (429, breaker cooldown) | "Exhausted" is failure-driven, not budget-driven |

## Decision

1. **Account = one `providers` row** (type + per-account config + one
   credential set in `provider_tokens`). Multiple accounts for a type are
   N rows sharing a `type` value. There is no separate accounts table.
   Per-type shared settings (codex issuer, ollama base_url) live in the
   config-KV under `provider_type_settings`; the effective provider config
   is type settings overlaid by the account's own config (account wins).
   Existing rows with no type settings behave exactly as before — no data
   migration.

2. **Chain entries are provider types OR concrete provider IDs.** At
   resolve time each entry is looked up as an exact provider ID first; a
   registered ID wins. Otherwise, if the entry names a known type, it
   expands to all registered accounts of that type in stable registry
   order. A type entry with no accounts is a traced skip, never a silent
   drop. New accounts therefore join the rotation without touching any
   rule; deleted accounts drop out the same way.

3. **Strategy: `round_robin` | `fill_first`, per rule with a global
   default of `round_robin`.** An empty rule strategy inherits the
   global setting (stored in the config-KV `routing_strategy`). Round
   robin rotates the expanded candidate list by a per-rule counter before
   capability and health filtering, distributing first-position load.
   Fill-first keeps today's semantics, named: sticky to the first entry
   until the health gate opens (breaker cooldown) or a 429 exhausts the
   same-provider retry budget and fails over (ADR-005 FO-2/FO-3). The
   executor is untouched: it receives the expanded, optionally rotated
   chain and its FO-1/FO-5 invariants hold by construction.

4. **OAuth draft accounts are excluded from expansion; keyless API-key
   accounts are not.** An abandoned codex draft (no token row) would route
   live traffic into a guaranteed auth failure, so the codex provider
   reports credential presence and the expansion skips credentialless
   accounts of that type — the UI surfaces them with a "sign-in required"
   badge instead. For api-key types (custom-openai, ollama-cloud) a missing
   key is a legal routing state by the existing construction contract
   (local auth-free endpoints), so their providers do not report credential
   presence at all and every constructed account joins the expansion.

5. **route_trace gains a `version` field** (`version: 2`; the previous
   unversioned `{"events":[...]}` shape is v1 retroactively). New event
   types `expand` and `rotation` are additive. The WebUI renders
   route_trace as raw JSON, so old rows keep rendering.

6. **OAuth stays the device flow** (RFC 8628): the existing
   `/oauth/start` + `/oauth/poll` admin endpoints, the SPA polling at
   the issuer's interval, tokens sealed server-side. `oauthStart` falls
   back to the type-settings issuer when the account's own config is
   empty (draft accounts).

## Consequences

- **ADR-005 determinism is restated, not broken**: resolution remains
  deterministic *per request sequence* — the round-robin counter is the
  only sequence-dependent state, it is per-rule, and rotation is fully
  observable in the trace (every rotation is an event). Testability is
  preserved: the counter makes distributions assertable exactly.
- The global default `round_robin` changes request distribution for
  existing installations (previously ordered = fill-first). Rules can
  pin `fill_first` to restore the old behavior; the release notes must
  say so.
- A provider whose ID equals a type string (legacy seed `ollama-cloud`)
  resolves as the exact ID, not as a type expansion — deliberate
  backward compatibility, documented.
- The executor's `MaxProviders` cap (default 3) still bounds attempts;
  long expanded chains are not fully walked. This keeps the ADR-005
  FO-3 research bounds; documented in routing-and-fallbacks.md.
- Type expansion happens on every resolve against the live registry, so
  account adds/deletes take effect at request time with no rule edits —
  and the trace shows the expansion, so the operator can see why a
  request landed on a given account.