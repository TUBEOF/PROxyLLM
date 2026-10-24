---
name: proxyllm-core
description: Use when starting any task in the PROxyLLM repository — locating code, choosing commands, or deciding where a change belongs — and before committing, testing, or writing docs for a change.
---

# PROxyLLM core map

Single-binary Go LLM proxy: three client surfaces (OpenAI-compatible,
Anthropic-compatible, decision models) over one canonical IR, so any harness
can run against any upstream model. One fact lives in exactly one place — this
skill is the map, `docs/` is the truth.

## Where things live

| I am touching… | Then |
|---|---|
| Wire shapes, codecs, streaming | `proxyllm-protocol`; code under `internal/translate/` |
| Upstream providers, accounts, keys | `proxyllm-provider`; `internal/provider/`, `internal/storage/` |
| Chains, fallbacks, health, emulated tools | `proxyllm-routing`; `internal/routing/` |
| Console pages, React/TS | `proxyllm-webui` + `proxyllm-design`; `webui/` |
| Gates, CI, releases, e2e | `proxyllm-ops` |

Layout: `internal/translate/ir` is the hub (every protocol is one codec pair);
`internal/routing` resolves ordered chains and records `route_trace`;
`internal/storage` is CGO-free SQLite with append-only migrations;
`internal/server/{httpapi,webui}` are the API surfaces; `testdata/golden/`
holds the wire fixtures; `design/` holds the console design system.

## Commands (packages are ALWAYS `./cmd/... ./internal/... ./webui` — never `./...`)

```bash
make build    make test    make test-race   # race is the gate for streaming work
make vet      make fmt     make lint        make check-tests
make webui-install webui-build              # then `make build` to embed
make webui-lint make webui-typecheck
```

One package: `go test ./internal/translate/...`. One test:
`go test ./internal/routing -run TestResolveChain -v`.

## Non-negotiables

- Tests derive from golden fixtures and the official specifications, never
  from this implementation's own output.
- A wire-shape change starts by re-reading the current official docs and
  updating the fixture's source URL and date.
- Every log call goes through `internal/observability/redact` — a log site
  that could emit a secret is a review rejection.
- Vendor wire facts (capability strings, error codes) live in configuration,
  not constants.
- Conventional commits, subject ≤ 72 chars, imperative, English.
- A `route_trace` schema change is additive and lands with the two tests that
  pin the version.
- Docs a change touches land in the same change (`docs/README.md` is the
  index; one fact, one page).

## Decide "where does this belong?"

| Question | Read |
|---|---|
| Why is it built this way? | `docs/adr/` (stable `ADR-NNN` ids cited in source) |
| Field X of protocol A → B? | `docs/reference/translation-matrix.md` |
| Compatibility setting? | `docs/reference/model-compat.md` |
| Config keys and layers? | `docs/configuration.md` |
| Endpoints and errors? | `docs/api/` (start at `README.md`) |