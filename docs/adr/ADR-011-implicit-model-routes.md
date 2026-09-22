# ADR-011: Implicit model routes (amending ADR-005's rule-only routing)

- **Status**: Accepted
- **Date**: 2026-09-22

## Context

Operating v1.1.0 surfaced a UX trap with a real support cost: a client
requests a model that is **listed** by `/v1/models` and visible in the
WebUI (e.g. `ollama-cloud/glm-5.3`, created by the ADR-010 model sync)
and receives a 404 whose route_trace says only
`{"type":"rejected","reason":"no matching routing rule"}`.

Root cause: two concepts both carry the name "model".

- The **models table** records existence: "this model is offered by that
  provider" (synced rows) or "the operator registered it here" (manual
  rows). `/v1/models` and the Models page render this table.
- A **routing rule** (ADR-005) records routing policy: which chain may
  serve which model pattern.

ADR-005 made rules the only route source — "never a silent default
model" — and seeded none. So every fresh installation lists models its
clients can never reach until an operator hand-writes a rule. The 404
carries no hint that a rule is the missing piece; the model list
actively advertises the unroutable models.

## Criteria

- P0: determinism — the same request must resolve to the same route
  every time; no guessing, no fuzzy scoring.
- P0: observability — every implicit route is a trace event; fallbacks
  and substitutions are never silent.
- P0: rules keep precedence — an explicit rule always wins over the
  implicit route; key scopes stay binding; aliases resolve first.
- P1: zero configuration for the common single-provider case (connect a
  provider, sync models, request works).
- P1: no storage change; no new operator setting (behavior is always on,
  documented here).

## Options considered

| Option | Strengths | Weaknesses |
|---|---|---|
| A. Keep rules-only; improve the 404 message | No behavior change | The trap stays: advertised-but-unroutable models, docs-first setup for every install |
| B. Filter `/v1/models` to routable models | List becomes honest | Hides models operators just synced; breaks model discovery for Claude Code before the first rule exists |
| C. Implicit route from the models row | Deterministic, zero-config, single source of truth for existence + provider | A request can route without any explicit routing decision — must not become a silent default |
| D. Auto-seed a rule per model | Explicit rule objects | Rule churn on every sync; collides with operator-edited rules of the same pattern |

## Decision

When no enabled routing rule matches the (post-alias) model name, the
router consults the models table: a row for that model ID resolves to an
implicit route with the row's provider as the chain. The route runs
through the unchanged pipeline — chain expansion, credential gate,
strategy, capability filter, health gate — so every skip is traced and
a dead or cooled-down provider still yields the honest
`chain empty after …` rejection, identical to an explicit rule.

Option C, guarded:

- **Rules win.** The implicit fallback fires only when the rule match
  found nothing (exact and wildcard tiers both empty). An explicit rule
  — even a low-priority wildcard — suppresses it entirely.
- **Key scopes win.** Step 1 rejection happens before the lookup; a
  scoped key cannot reach a model through the implicit path.
- **Aliases win.** The lookup uses the post-alias name, so aliases keep
  their resolution semantics unchanged.
- **Never silent.** Every implicit route is traced as
  `implicit_route` (route_trace v5), carrying the model ID and the
  provider. The trace makes the routing decision reviewable in the
  request log — the WebUI shows the raw trace.
- **The row is the operator's decision, not a guess.** A models-table
  row exists only because the operator connected the provider and ran
  the sync (or wrote the row by hand). Routing to the provider the row
  names is the deterministic resolution of that decision, not a
  fabricated default model — ADR-005's "never a silent default" is
  about not inventing routes, and nothing is invented here.

The lookup is injected as a router option (`WithModelLookup`,
`func(modelID string) (providerID string, ok bool)`) so the routing
package keeps its layering (no storage import); `ok=false` on a missing
row or a store read failure degrades to the unchanged no-route
rejection — the last-known-good read contract.

## Consequences

- A fresh install works with zero rules: connect a provider, sync, and
  model requests route. Rules become what they always meant to be —
  policy (fallback chains, priorities, strategies) — not a prerequisite
  for basic operation.
- The "no matching routing rule" 404 now means exactly "unknown model"
  (no row, no rule) — the error message keeps its name; docs updated.
- Route_trace schema bumps to v5 (`implicit_route` event). Additive —
  the WebUI renders raw JSON, old rows render unchanged.
- Multi-provider setups where operators previously relied on "no rule →
  no routing" as a kill switch must use key scopes or delete/disable
  the model rows instead. Documented in routing-and-fallbacks.md.
- Web-tool account IDs can never be implicit targets: they do not
  implement model discovery, are never synced, and a hand-written row
  pointing at one still hits the unchanged "provider not registered"
  skip (web-tool rows are not LLM-chain-routable, ADR-010).