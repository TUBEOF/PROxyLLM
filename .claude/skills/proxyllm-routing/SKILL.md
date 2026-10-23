---
name: proxyllm-routing
description: Use when changing PROxyLLM's request path — routing rules, fallback chains, health tracking, capability gaps, emulated server tools (web_search, web_fetch, analyze_image), image captioning, or the route_trace schema.
---

# PROxyLLM routing layer

`internal/routing` resolves each request through an ordered rule chain
(ADR-005), walks it with health-aware failover, and records every transition in
`route_trace` (schema in `docs/reference/route-trace.md`, versioned — additive
changes land with the two tests that pin the version).

## The map

| Concern | Read |
|---|---|
| Rule model, chain order, implicit routes | `docs/routing-and-fallbacks.md` |
| Why deterministic chains, health cooldowns | `docs/adr/ADR-005`, `ADR-009`, `ADR-011` |
| Context-window-aware fallbacks | `docs/adr/ADR-013` |
| Vision fallback + `analyze_image` tool | `docs/adr/ADR-018`, `docs/adr/ADR-014` |
| Web-tool emulation resilience | `docs/adr/ADR-015` |
| Capability learning (vision, tools) | `internal/server/capabilityfallbacks.go` |

## Hard rules

- Failover is deterministic: round-robin counters are the only
  sequence-dependent state, per-rule and observable.
- Mid-stream failures are terminal: an error after the first streamed event
  goes to the client, never to the next chain entry.
- Emulated server tools answer through the web-tool accounts with budgets
  (`max_uses`, total/call caps) and the loop's `{"error_code"}` shape —
  `internal/routing/emulation.go`.
- The image bootstrap caption is task-independent, once per image
  (ADR-018 addendum); the reactive rescue keeps task awareness.
- Vendor capability strings and error codes live in configuration, not
  constants.

## Changing the trace schema

Additive only. Add the field, bump the version, and land the change together
with the two tests that pin the new version. The console renders the trace, so
`webui/` may need the matching render — check `docs/reference/route-trace.md`
first.

## Verify

```bash
go test ./internal/routing/...          # includes emulation + caption tests
make test-race                          # routing is streaming-adjacent
```

Live, against the local instance (127.0.0.1:18080): rebuild the binary via the
test-instance script first, then inspect `request_logs` / trace rows read-only.