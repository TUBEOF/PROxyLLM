# PROxyLLM design system — the short entry point

The console under `webui/src` is styled by one token layer and one component
layer. This file is the minimum you need before touching either.

## Read this first

- **`DESIGN.md`** — the rules: colour and its roles, typography, spacing,
  components, motion, do/don't. Authoritative for prose.
- **`brand.json`** — the same system in machine-readable form: every token with
  its role, the voice, the component rules, the anti-patterns.
- **`colors_and_type.css`** — the token source. `webui/src/styles/colors_and_type.css`
  is the runtime copy the console loads and must stay byte-identical to it (a Go
  test in `webui/` fails if the two drift).
- **`.claude/skills/proxyllm-design/`** — the same material as an agent skill,
  loaded automatically when you work under `webui/**`.

## Where things live

| Path | What it is |
|---|---|
| `design/colors_and_type.css` | token source — every colour, type, radius, shadow, motion and z-index value |
| `design/brand.json` | machine-readable profile of the same system |
| `design/examples/` | two standalone pages: the tokens, and the components |
| `design/fonts/README.md` | which fonts ship, where, under which licence |
| `design/assets/brand/wordmark.svg` | the wordmark |
| `webui/public/fonts/` | the one font source, served at `/fonts/` |
| `webui/src/styles/kit.css` | component layer: the classes pages compose from |
| `webui/src/styles/app.css` | the two app-local additions (loading state, login card) |

## How to build a page

1. Import nothing: the stylesheets are loaded once in `webui/src/main.tsx`
   (`kit.css`, then `app.css`). A route component composes classes.
2. Compose from the kit: `.appshell`, `.widget` + `.widget-head`/`.widget-body`,
   `.kpi`, `.table-wrap`/`.table`, `.field`/`.input`, `.btn`, `.badge`, `.pill`,
   `.modal`. The inventory with every class is in
   `.claude/skills/proxyllm-design/references/components.md`.
3. Colour, size and motion come from tokens — never a literal in a route
   component or in `app.css`.
4. Missing data renders an em dash or `.empty`, never a plausible-looking number.
5. Icons are inline SVG from `webui/src/icons.tsx`; no emoji.

## Non-negotiables

- Dark only. No light theme, and nothing reads `prefers-color-scheme: light`.
- One chromatic accent (green). Status tones are the only other hues.
- Never override a token in a component rule; add a token instead, next to the
  ones it belongs with.
- Keep the app pixel-identical when you refactor styles: `e2e/design-rules.spec.ts`
  must stay green and the Playwright screenshot baselines are rendered in CI, so
  a change that is meant to look the same must not move a pixel.

## Before you push

```bash
cd webui && npm run lint && npm run typecheck && npm run build
cd .. && go test ./webui
```

and run the design-rule assertions:

```bash
go build -o webui/e2e/fixturesrv-bin ./webui/e2e/fixturesrv
cd webui && npx playwright test -c playwright.config.ts --project=desktop-chrome e2e/design-rules.spec.ts
```
