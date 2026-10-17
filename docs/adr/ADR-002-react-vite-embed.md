# ADR-002: WebUI — React 18 + Vite + TypeScript, embedded via go:embed

- **Status**: Accepted
- **Date**: 2026-09-10

## Context

The WebUI must cover OAuth flow UI (start/poll/callback states), live
stats, log viewer with route_trace detail, routing/compat editor — plus fit
the console's design system (pure CSS/HTML component kit, dark-only, single
green accent). It must embed into the single Go binary.

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
`go:embed`. Design contract: `design/colors_and_type.css` verbatim as token
layer (its runtime copy under `webui/src/styles/`), `webui/src/styles/kit.css`
as component layer (class names unchanged), fonts self-hosted (SIL OFL). No UI
framework library.

Embedding pattern (verified live 2026-09-10, Flipt engineering blog):
`//go:embed all:dist/*` — the `all:` prefix includes `_`/`.`-prefixed
assets — adapted into `http.FS()` and served with an SPA fallback to
`index.html` for client-side routes.

## Consequences

- Bundle budget ≤ 200 KB gzip initial load, CI-enforced.
- Visual regression tests (Playwright) against the console's own baselines:
  the design-rule assertions block a PR, the screenshot comparison reports
  (diff artifact + one sticky PR comment).
- Plain React SPA with an SPA-fallback handler — no SSR framework, no
  Next.js; the Flipt post-mortem confirms SSR export formats fight the
  embed.FS model.

## Implementation notes

**Embed shape.** The directive lives in `webui/embed.go` as
`//go:embed all:dist` — without a trailing `/*`: patterns are resolved
relative to the package directory and `all:dist` already covers every file
recursively, including `.`/`_`-prefixed ones. `dist/` is a build artifact
except for a committed placeholder `dist/index.html`, so a fresh clone
compiles without a Node toolchain (go:embed fails the build on a pattern
matching no file) and CI runs `npm ci && npm run build` before `go build`.

**Router library: react-router-dom 6.30.1, library mode (not framework
mode).** React Router 7's "framework mode" bundles its own build pipeline
(SSR/loader conventions, `react-router.config.ts`); RR7 library mode is the
6.x API carried forward. For a Vite SPA embedded via embed.FS the library
mode of the stable v6 line is the smallest, best-understood surface — RR7
framework mode would fight the embed model exactly like Next.js would
(ADR decision above). History routing, not hash routing: the Go-side SPA
fallback serves `index.html` for browser
navigations that match no file, so deep links survive hard refreshes and
the CSP needs no `unsafe-inline`.

**Inline styles are not CSP `style-src` violations.** React's `style={{}}`
props are applied through the CSSOM (`element.style.setProperty`), not by
emitting a `<style>` element or a `style:` attribute string — the
`style-src` directive governs `<style>` elements and `style` attributes,
so `'self'`-only is correct for the SPA. Verified against MDN's CSP
documentation and the CSP spec's matching algorithm.
An `<img src="data:">` would still need `img-src data:`;
the routes use inline SVG components instead, so the shipped CSP keeps
`data:` in `img-src` only as documented headroom.