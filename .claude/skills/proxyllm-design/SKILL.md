---
name: proxyllm-design
description: Use when working on anything under webui/ — building or restyling a page, route or component of the PROxyLLM console, choosing a colour, size or spacing value, adding a component, or reviewing whether a UI change stays inside the design system and passes its design-rule test. Covers the token layer, the component kit, the app shell, interaction states, accessibility and the design-rule suite.
paths: "webui/**"
---

# PROxyLLM console design system

The console is a dark-only operational UI: one green accent for health and
action, glass surfaces on a near-black canvas, Plus Jakarta Sans headings over
Inter body text, JetBrains Mono for numbers. Its rules live in
`design/DESIGN.md` (prose) and `design/colors_and_type.css` (tokens); the CSS the
app loads is `webui/src/styles/`.

## Read the right file

| Question | File |
|---|---|
| Which token for this colour, and is it allowed? | `references/tokens.md` |
| Which class do I use, and what does it render? | `references/components.md` |
| Page shell, grid, gutter, breakpoints, density | `references/layout.md` |
| hover / focus / active / disabled / loading / empty / error / running / healed | `references/states.md` |
| Focus, motion, contrast, keyboard, aria | `references/accessibility.md` |
| Everything, from the app's own rules | `../../../design/DESIGN.md` |

## Non-negotiables

- **Tokens only in page code.** `app.css` and everything under `webui/src`
  reference custom properties or `currentColor`. A value that needs a colour of
  its own earns a token in `design/colors_and_type.css` — added to both copies.
- **Dark only.** No light theme; nothing reads `prefers-color-scheme: light`.
- **One chromatic accent.** Green. Status tones (`--status-warn/down/paused/info`)
  are the only other hues and they carry state, never decoration.
- **Honest data.** A missing value is `—` or an `.empty` state, never a plausible
  number.
- **Icons are inline SVG** from `webui/src/icons.tsx`; never emoji.
- **A style refactor must not move a pixel.** `e2e/design-rules.spec.ts` stays
  green, and the screenshot baselines (rendered in CI) stay unchanged.

## Verify a change

```bash
cd webui && npm run lint && npm run typecheck && npm run build
cd .. && go test ./webui                        # token source == runtime copy
go build -o webui/e2e/fixturesrv-bin ./webui/e2e/fixturesrv
cd webui && npx playwright test -c playwright.config.ts --project=desktop-chrome \
  e2e/design-rules.spec.ts                       # dark-only, tokens-only, no emoji
```

Then `git checkout webui/dist/index.html` — the committed placeholder must stay.

## Common mistakes

| Mistake | What happens | Fix |
|---|---|---|
| A hex or `oklch()` literal in a route component or `app.css` | The hue-window check in `design-rules.spec.ts` flags it, or a future palette change misses it | Use a token; if none fits, add one to the token source and its runtime copy |
| Declaring a token inside a component rule (`.logs-table{--rail:…}`) | The value exists only where that one rule runs, and the sync test cannot see it | Declare it in `colors_and_type.css`, both copies |
| Editing `webui/src/styles/colors_and_type.css` alone | `webui/design_sync_test.go` fails | Edit `design/colors_and_type.css`, then copy the file over |
| A second accent for a chart series or a new badge | Two systems in one screen | `--chart-2` (neutral) for a comparison series, a `--status-*` tone for state |
| Reading `useMutation().error` right after `run()` | The previous mutation's error renders | Branch on the thrown error or on `run()`'s null return |
| Variable-length text on a `flex-wrap` line above other content | Font-metric drift between machines moves the wrap point and everything below shifts a line | Block layout |
| Adding a font file to `design/fonts/` | Two font sources that drift | Files live in `webui/public/fonts/`; `design/fonts/README.md` points there |
