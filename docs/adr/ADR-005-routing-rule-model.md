# ADR-005: Routing — deterministic rule model

- **Status**: Accepted
- **Date**: 2026-09-10

## Context

Routing decides, per request, which provider/model chain serves it:
capability-based fallback, health-aware failover, Claude Code alias
resolution, per-model compatibility policies (Analysis E).

## Decision

**Deterministic, ordered rule model** (state in DB, WebUI-editable):

1. **Key scope** — client key's allowed model patterns.
2. **Model match** — exact, wildcard, alias map (Claude Code aliases →
   configured primary models).
3. **Capability filter** — from request content: web_search, web_fetch,
   code_execution, streaming, thinking. Vision is deliberately NOT gated
   here (2026-09-21): a vision request goes out, and a real upstream
   rejection fails over. The context window is likewise not gated on the
   rule chain — the fallback walk gates on it instead (ADR-013).
4. **Chain resolution** — ordered provider/model fallback chain; providers
   failing the capability filter are skipped with a logged reason.
5. **Health gate** — circuit breaker (consecutive failures), exponential
   backoff, probe recovery; cooled-down providers skipped/deprioritized.

**Failover semantics**: request-level retry only before the first streamed
byte; once streaming, upstream failure terminates with a protocol-conform
error event — no mid-stream provider swap. Retries on connection errors,
429, 5xx honoring `Retry-After`. Every transition (skip, retry, failover,
emulated-tool substitution, compat-policy application) is recorded in
`route_trace` JSON in request_logs + WebUI request detail. **Fallbacks are
never silent.**

## Consequences

- Deterministic = testable: table-driven tests cover capability fallback,
  health cooldown, mid-stream no-swap, compat-policy application.
- Fuzzy score/weight routing was rejected as the primary mechanism
  (non-deterministic, hard to test); ML-ish routing out of scope.
- The route_trace contract is a public observability surface — the WebUI
  log viewer renders it, so its schema is versioned.