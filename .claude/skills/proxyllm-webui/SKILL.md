---
name: proxyllm-webui
description: Use when changing the PROxyLLM console SPA — React routes, data fetching, admin API calls, mutations, live updates, or the embedded build — beyond purely visual styling (which proxyllm-design covers).
---

# PROxyLLM console app

React + Vite + TypeScript under `webui/src`, embedded into the single binary.
**For anything visual — colour, spacing, classes, states, a11y — load
`proxyllm-design` and read `design/DESIGN.md` first.** This skill covers the
app's data flow and the rules that have broken releases before.

## Structure

Routes under `webui/src/routes/` (one file per console page), shared components
under `webui/src/components/`, API helpers (`apiCall`, `useLiveData`,
`useAdminData`) around `webui/src/api.ts`, types mirror the admin DTOs in
`webui/src/types.ts`. The console's pages are documented in
`docs/operating.md`; the admin API in `docs/api/admin.md`.

## Non-negotiables

- `useMutation().error` right after `run()` is the PREVIOUS mutation's error —
  branch on the thrown error or `run()`'s null return.
- Never put variable-length text in a flex-wrap line above other content —
  font-metric drift shifts everything below. Block layout.
- The bundle budget is 200 KB gzip for the initial load, enforced in CI. The
  Logs view is lazily loaded and does not count against it.
- `webui/dist/index.html` in git is a committed placeholder. After any SPA
  build run `git checkout webui/dist/index.html`, and confirm
  `git diff origin/main -- webui/dist/index.html` is empty before committing.
- Runtime style copies under `webui/src/styles/` are kept in sync with
  `design/` by a Go test — edit the `design/` source, then copy over.
- Changing rendered UI moves the visual baselines: refresh them only through
  the `update-visual-baselines` workflow (CI), never by hand.

## Verify

```bash
cd webui && npm run lint && npm run typecheck
go build -o webui/e2e/fixturesrv-bin ./webui/e2e/fixturesrv   # re-embed first
cd webui && npx playwright test -c playwright.config.ts --project=desktop-chrome \
  e2e/<touched-spec>.spec.ts
```

The assertion suite runs anywhere; screenshot baselines are CI-rendered.