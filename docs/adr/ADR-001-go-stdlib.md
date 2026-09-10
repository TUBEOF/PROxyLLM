# ADR-001: Backend — Go + stdlib net/http

- **Status**: Accepted
- **Date**: 2026-09-10

## Context

PROxyLLM is a streaming-first LLM proxy: long-lived SSE connections,
bidirectional protocol translation, fallback routing, a single-service
deployment story, and an open-source contributor base. The backend choice
dominates every downstream property.

## Criteria (from Analysis A)

- First-class SSE streaming with backpressure (P1)
- Concurrency for many long-lived streams (P1)
- Minimal single-service container / static binary (P1)
- Static analysis & testing ecosystem
- SDK/HTTP maturity for both wire protocols
- OSS contributor accessibility
- Latency/memory footprint (P1)

## Options considered

Scored 1–5 per criterion, P0/P1 double-weighted:

| Criterion | Go + stdlib | Rust + axum | TypeScript | Python |
|---|---|---|---|---|
| SSE streaming & backpressure | 5 | 5 | 3 | 2 |
| Concurrency model | 5 | 5 | 4 | 2 |
| Single static binary | 5 | 5 | 2 | 2 |
| Static analysis & testing | 5 | 5 | 4 | 4 |
| SDK maturity | 5 | 3 | 5 | 5 |
| OSS contributor pool | 5 | 2 | 4 | 4 |
| Latency/memory | 5 | 5 | 3 | 2 |
| **Total (weighted)** | **44** | **35** | **29** | **23** |

## Decision

**Go + stdlib `net/http`** — Go 1.22+ `http.ServeMux` with method+path
patterns; middleware as `func(http.Handler) http.Handler` chains. Wire
protocols handled with `encoding/json` + bufio SSE parsing. Official SDKs
exist (anthropic-sdk-go, openai-go) but raw HTTP is ideal for a proxy that
must own header passthrough and SSE framing.

## Consequences

- Goroutines + channels + `http.Flusher` give natural per-stream
  backpressure and cancellation via `context`.
- CGO-free pure-Go build keeps the single static binary + distroless
  container story intact.
- We own more wire-protocol code than an SDK would give us — accepted in
  exchange for full control over passthrough semantics (gateway protocol
  requirements C3/C4).
- Some protocol churn maintenance (e.g., dated server-tool strings A8) is
  unavoidable in any stack; mitigated by config-driven mapping tables.