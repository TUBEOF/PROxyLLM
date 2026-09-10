# ADR-002: WebUI — React 18 + Vite + TypeScript, embedded via go:embed

- **Status**: Accepted
- **Date**: 2026-09-10

## Context

The WebUI must cover OAuth flow UI (start/poll/callback states), live
stats, log viewer with route_trace detail, routing/compat editor — plus fit
the UpGuard design system (pure CSS/HTML dashboard kit, dark-only, single
green accent). It must embed into the single Go binary (Analysis B).

## Options considered

| Criterion | React + Vite + TS | Preact | Svelte | Go templates + htmx |
|---|---|---|---|---|
| Kit integration | 5 | 4 | 2 | 4 |
| Bundle budget | 4 | 5 | 5 | 5 |
| Interactive surface | 5 | 5 | 5 | 2 |
| Contributor pool | 5 | 3 | 3 | 3 |
| Embedding | 5 | 5 | 5 | 5 |
| **Total** | **24** | **22** | **20** | **19** |

## Decision

**React 18 + Vite + TypeScript**, built to static assets, embedded via
`go:embed`. Design contract: `colors_and_type.css` verbatim as token layer,
`ui_kits/dashboard/kit.css` as component layer (class names unchanged),
fonts self-hosted (SIL OFL). No UI framework library.

Embedding pattern (verified live 2026-09-10, Flipt engineering blog):
`//go:embed all:dist/*` — the `all:` prefix includes `_`/`.`-prefixed
assets — adapted into `http.FS()` and served with an SPA fallback to
`index.html` for client-side routes.

## Consequences

- Bundle budget ≤ 200 KB gzip initial load, CI-enforced.
- Visual regression tests (Playwright) against UpGuard kit baselines are a
  blocking CI gate (Analysis F doctrine).
- Plain React SPA with an SPA-fallback handler — no SSR framework, no
  Next.js; the Flipt post-mortem confirms SSR export formats fight the
  embed.FS model.