---
name: PROxyLLM Console Design System
description: >-
  The design system of the PROxyLLM console: a dark-only operational interface
  on a near-black canvas with one green accent for health and action, glass
  surfaces, Plus Jakarta Sans headings over Inter body text, and JetBrains Mono
  for the numbers an operator reads at a glance. Machine-readable profile:
  brand.json. Token source: colors_and_type.css. Agent entry point: AGENTS.md.
version: 2.0.0
mode: dark-only
sources:
  tokens: colors_and_type.css
  profile: brand.json
  component_layer: webui/src/styles/kit.css
  app_layer: webui/src/styles/app.css
colors:
  bg: { value: "oklch(11% 0.011 250)", srgb: "#030507", role: "canvas — page background" }
  surface: { value: "oklch(13% 0.012 250 / 80%)", role: "glass card, widget, table and pill surface + blur(10px)" }
  surface-elevated: { value: "oklch(13% 0.013 250 / 96%)", role: "modal, drawer, menu, toast backing" }
  fg: { value: "oklch(90% 0.008 250)", srgb: "#dadee3", role: "primary text" }
  muted: { value: "oklch(81% 0.012 250)", srgb: "#bbc2c9", role: "secondary text, nav, captions" }
  border: { value: "oklch(28% 0.015 250)", srgb: "#232a30", role: "hairlines, card borders, dividers" }
  accent: { value: "oklch(78% 0.20 144)", srgb: "#56d75b", role: "action, focus, links, healthy status, the logo dot" }
  accent-dim: { value: "oklch(78% 0.20 144 / 12%)", role: "soft accent fill: chips, icon circles, menu hover" }
  status-up: { value: "var(--primary-500)", role: "2xx / healthy" }
  status-warn: { value: "oklch(78% 0.15 75)", srgb: "#efa831", role: "degraded, retry, failover" }
  status-down: { value: "oklch(60% 0.20 25)", srgb: "#de3b3d", role: "4xx/5xx, failure, destructive" }
  status-paused: { value: "oklch(62% 0.015 250)", srgb: "#80878f", role: "paused, inactive" }
  status-info: { value: "oklch(72% 0.09 230)", srgb: "#65afd2", role: "informational, sub-operation" }
  rail: { value: "oklch(70% 0.17 143 / 72%)", role: "request-log tree rail: parent → sub-operation" }
  text: { value: "var(--neutral-300)", role: "resting text colour inside a data cell" }
  bg-elev: { value: "rgba(127, 127, 127, 0.12)", role: "achromatic wash marking a moved row" }
typography:
  display: { family: "Plus Jakarta Sans", weights: "variable, declared 200–800", role: "headings, page titles, KPI values, wordmark" }
  body: { family: "Inter", weights: "variable, declared 1–1000", role: "body text, tables, forms, buttons, nav" }
  mono: { family: "JetBrains Mono", weights: "400, 500", role: "numbers, ids, client keys, timestamps, pills" }
  scale: { xs: "0.75rem", sm: "0.875rem", base: "1rem" }
  body_size: 16px
  line_height: 1.5
spacing:
  base_unit: 4px
  rhythm: [2, 4, 6, 8, 10, 12, 14, 16, 18, 20, 22, 24, 28, 32, 36, 40]
  page_gutter: 24px
  widget_gap: 16px
  table_cell: "13px 16px"
radius:
  sm: 6px
  md: 8px
  lg: 12px
  full: 9999px
shadows:
  glass: "inset 0 1px 1px 1px oklch(100% 0 0 / 15%)"
  card_hover: "0 8px 25px oklch(78% 0.20 144 / 15%)"
  btn_primary: "inset 0 -4px 4px oklch(0% 0 0 / 10%), 0 0 14px oklch(100% 0 0 / 19%)"
  modal: "0 24px 60px oklch(0% 0 0 / 50%)"
  drawer: "-20px 0 50px oklch(0% 0 0 / 40%)"
motion:
  base: "0.15s cubic-bezier(0.4, 0, 0.2, 1)"
  button: "0.3s ease-out"
  sidebar: "0.25s ease-out"
  status_ping: "2s cubic-bezier(0, 0, 0.2, 1) infinite"
  running_dot: "1.2s ease-in-out infinite"
  spinner: "0.9s linear infinite"
components:
  app_shell: "sidebar 240px (collapsed 64px) + topbar 72px + routed page; 12-column widget canvas, 16px gap"
  buttons: "primary (accent gradient, dark text), secondary (surface outline), ghost (accent outline), danger (status-down), small, icon-only, round, group"
  cards: "widget: surface + border + blur(10px), 12px radius; border turns to --neutral-600 on hover, accent border + glow on interactive cards"
  tables: "glass table, 13px/16px cells, row hover --row-hover, sticky header, mono right-aligned numbers, pills for provider/protocol, badges for status"
  forms: "label above field, 40px input, focus = accent border + 3px accent halo, error = status-down border + inline message"
  overlays: "modal (backdrop 60% + blur(4px)), drawer — rail 420px or the 80% panel, user menu 210px, toast, tooltip"
do_not:
  - "Never introduce a second chromatic colour — the accent is green; status tones are the only other hues."
  - "Never ship a light theme or read prefers-color-scheme: light — dark is the only appearance."
  - "Never fabricate a number: a value with no data renders an em dash or an empty state."
  - "Never put emoji in the interface — icons are inline SVG."
  - "Never hot-link a remote asset — fonts and images are served from this repository."
  - "Never override a token in a component rule, and never write a colour literal outside the token layer unless no token expresses the nuance."
---

# PROxyLLM — Console Design System

The console is the operator-facing surface of the proxy: request logs, routing,
providers, models, quota, client keys. It is read at a glance during an
incident, so the system optimises for **scan-ability on a dark screen**: one
accent that means *healthy* or *act*, hairline-separated density instead of
whitespace, and numbers in a monospaced face so columns line up.

Everything in this document is derived from what the console renders.
`colors_and_type.css` is the token source, `webui/src/styles/kit.css` is the
component layer, and `.claude/skills/proxyllm-design/` carries the same material
in agent-readable form. If this document and the console ever disagree, the
console is right and this document has a bug.

## 1. Colour

Authored in `oklch`. The values are the source of truth; the sRGB column is what
a browser composites on screen, and it is only ever an approximation of the
token — never a value to copy into a rule.

### The canvas and its surfaces

| Token | Value | sRGB | Role |
|---|---|---|---|
| `--bg` | `oklch(11% 0.011 250)` | `#030507` | the canvas. A near-black with a faint cool tint, not a mid grey and not pure black |
| `--surface` | `oklch(13% 0.012 250 / 80%)` | — | glass cards, widgets, tables, the sidebar: one step above the canvas, blurred by 10px |
| `--surface-elevated` | `oklch(13% 0.013 250 / 96%)` | — | anything that floats over the page and must stay readable: modal, drawer, user menu, toast |
| `--border` | `oklch(28% 0.015 250)` | `#232a30` | the hairline that does the separating, because surfaces are 2% apart |
| `--neutral-950 … --neutral-200` | ramp | see §6 | the canvas ramps up through surfaces to text; every step is a lighter layer, never a different hue |

There is no elevation beyond `--surface-elevated`: depth is expressed by
opacity and blur, not by stacked shadows.

### Text

| Token | Value | sRGB | Role |
|---|---|---|---|
| `--fg` | `oklch(90% 0.008 250)` | `#dadee3` | primary text, high-emphasis UI |
| `--muted` | `oklch(81% 0.012 250)` | `#bbc2c9` | secondary text: nav links, captions, help text, table meta |
| `--neutral-400/500` | ramp | `#989fa8` / `#6c7680` | tertiary: widget headers, placeholders, disabled, the dimmest text the system uses |
| `--text` | `var(--neutral-300)` | `#bbc2c9` | the resting colour of a number inside a data cell — the cell's own colour, named so the request-log token column can point at it |

### The accent

One chromatic colour, in two steps. Green means *healthy* (2xx, a served
request, a live connection) and *act* (primary button, focus ring, links).

| Token | Value | sRGB | Role |
|---|---|---|---|
| `--accent` / `--primary-500` | `oklch(78% 0.20 144)` | `#56d75b` | CTAs, focus, links, status-up, the sidebar dot |
| `--primary-400` | `oklch(80% 0.22 145)` | `#45e159` | the lighter end of the primary-button gradient only |
| `--primary-500-alpha` | `oklch(78% 0.20 144 / 35%)` | — | translucent accent: the border of an interactive card on hover |
| `--accent-dim` | `oklch(78% 0.20 144 / 12%)` | — | soft fills: chips, icon circles, menu-item hover |
| `--rail` | `oklch(70% 0.17 143 / 72%)` | — | the request-log tree rail — the accent, quieter, so the parent → sub-operation lines read as structure |

### Status

Status is the only place a second hue is allowed, and status hues never appear
as decoration.

| Token | Value | sRGB | Role |
|---|---|---|---|
| `--status-up` | `var(--primary-500)` | `#56d75b` | 2xx, healthy, online |
| `--status-warn` | `oklch(78% 0.15 75)` | `#efa831` | degraded, retried, failed over, 4xx |
| `--status-down` | `oklch(60% 0.20 25)` | `#de3b3d` | 5xx, failed, destructive action |
| `--status-paused` | `oklch(62% 0.015 250)` | `#80878f` | paused, inactive, unknown |
| `--status-info` | `oklch(72% 0.09 230)` | `#65afd2` | informational: sub-operations, capability fallbacks, image captions |

### Data surfaces

| Token | Value | Role |
|---|---|---|
| `--chart-line` | `var(--primary-500)` | the single data series |
| `--chart-area` | `oklch(78% 0.20 144 / 18%)` | the fill under a sparkline — charts are filled, never outlines |
| `--chart-2` | `var(--neutral-400)` | a comparison series: neutral, never a second accent |
| `--metric-pos` / `--metric-neg` | accent / `--status-down` | the sign of a delta |
| `--row-hover` | `oklch(14% 0.013 250 / 60%)` | table row hover |
| `--widget-head` | `var(--neutral-400)` | widget header text |
| `--bg-elev` | `rgba(127, 127, 127, 0.12)` | achromatic wash marking a row that is being moved |

## 2. Typography

Three families, three jobs. All three are self-hosted woff2 under
`webui/public/fonts/` (see §7).

| Role | Family | Weights | Where |
|---|---|---|---|
| Display | **Plus Jakarta Sans** | 600, 700 | page title (26px), KPI value (34px), widget and modal titles, drawer title, login heading, the wordmark |
| Body | **Inter** | 400, 500, 600 | everything else: table cells, forms, buttons, nav, drawers |
| Mono | **JetBrains Mono** | 400, 500 | numbers, token counts, latencies, client keys, timestamps, pills, the raw-trace block |

Sizes: `--fs-xs` 12px (pills, table meta, captions), `--fs-sm` 14px (tables,
nav, buttons, drawer text), `--fs-base` 16px (body, provider cards). Weight
`--fw-medium` 500 marks a label or a table's primary cell, `--fw-semibold` 600 a
heading, `--fw-bold` 700 the wordmark. Body text is 16px / 1.5.

Mono text is right-aligned in tables and carries `font-feature-settings:"tnum"`
(`.num`), so a column of figures lines up digit for digit.

## 3. Spacing and density

The base unit is 4px, and the numbers that recur are its multiples: 2, 4, 6,
8, 10, 12, 14, 16, 18, 20, 22, 24, 28, 32, 36, 40. There is no spacing token
scale — the component layer composes these values directly, and the rhythm is
what the numbers below describe:

| Measure | Value |
|---|---|
| Page gutter | 24px |
| Widget canvas gap | 16px (12-column grid, `w-1` = 3 cols, `w-2` = 6, `w-3` = 9, `w-full` = 12) |
| Widget padding | 18px body, 14px 18px header |
| Table cell | 13px 16px |
| Sidebar | 240px, collapsed 64px; nav padding 12px, section label 14px 12px 6px |
| Topbar | 72px, padding 0 24px, 16px between items |
| Feed row | 11px 0, 12px gap |
| Status grid | `minmax(54px, 1fr)`, 8px gap |

Density is deliberate. The request log at 1280×720 shows the fields an operator
compares across rows — status, model, provider, account, key, tokens, TPS,
latency, time — in one line, and a sub-operation row keeps its parent's column
count so the eye can read down the table.

## 4. Radii, shadow, blur, motion

- Radii: `--radius-sm` 6px (inputs, chips, small controls), `--radius-md` 8px
  (menus, icon buttons), `--radius-lg` 12px (cards, modals, drawers),
  `--radius-full` 9999px (pills, status dots, round buttons).
- Shadow is used for glass and for light, not for depth: `--shadow-glass` is the
  inset highlight that makes a blurred surface read as glass; `--shadow-card-hover`
  is the accent glow of an interactive card; `--shadow-btn-primary` /
  `--shadow-btn-hover` give the primary button its pressed bottom edge and its
  glow. Overlays use a large soft shadow to separate from the page.
- Blur: `--blur-card` 10px on card and overlay surfaces, `--blur-glass` 2.5px on
  the sticky topbar.
- Motion is short and functional: `--transition-base` 0.15s for colour and
  background, `--transition-button` 0.3s for the primary button's glow, 0.25s for
  the sidebar collapse. Three animations exist and all three carry state, not
  decoration: `status-ping` (2s) marks the sidebar's live dot and a healthy
  status dot, `b-dot-pulse` (1.2s) marks a request that is still running, and the
  route-level spinner (0.9s) marks a route chunk that has not loaded.
- `prefers-reduced-motion: reduce` neutralises every duration in
  `colors_and_type.css` and switches off the ping and the spinner; the labels
  still carry the state on their own.

## 5. Components

The component layer is `webui/src/styles/kit.css`; the classes below are the
ones the console renders. `app.css` adds only the route-level spinner and the
login card, and overrides nothing.

- **App shell** — `.appshell` grid: sidebar + content. `.sidebar` (240px,
  `.collapsed` 64px icon rail) with `.sidebar-brand` (accent dot + wordmark),
  `.sidebar-section` labels, `.sidebar-link` with the accent left indicator on
  `.active`. `.topbar` (72px, sticky, glass) with the collapse toggle and the
  account avatar.
- **Widget** — the page's content unit: `.widget` with `.widget-head` (title,
  optional actions) and `.widget-body`. Sizes `.w-1/.w-2/.w-3/.w-full` on the
  12-column `.canvas`.
- **KPI** — `.kpi` with `.kpi-label`, `.kpi-value` (display face, 34px),
  `.kpi-foot` and a `.delta` badge (`pos` accent, `neg` red, `neu` neutral).
- **Status** — `.badge` with `.up/.warn/.down/.paused/.info` and a `.b-dot`;
  `.status-dot`, `.mini` tiles in `.status-grid`, and the `.health-ring` score.
- **Charts** — `.spark` (area + line + last-point dot), `.bars` with `.bar-track`
  /`.bar-fill`, `.ring` with `.ring-seg`/`.ring-track`, `.heat` cells. Fills, not
  outlines; one accent series plus neutral comparisons.
- **Table** — `.table-wrap` → `.table-scroll` → `.table`, with `.primary`,
  `.mono`, `.num`, `.row-actions`, `.table-toolbar`, `.pagination`, `.selbar`.
- **Forms** — `.form`, `.field` (label above, `.hint`, `.error`, `.is-error`,
  `.is-valid`), `.input`, `.textarea`, `.select`, `.input-affix`, `.fieldset`,
  `.form-row`, `.form-actions`, and `.form-note` for the error line a form shows
  instead of an alert.
- **Controls** — `.check` + `.cbox`, `.toggle` + `.tswitch`, `.radio` + `.rdot`,
  `.rpills` + `.rpill`. Each hides a real input and styles the sibling, so
  keyboard and assistive technology see a normal control.
- **Tabs and breadcrumb** — `.tabs`/`.tab`, `.breadcrumb`.
- **Overlays** — `.modal-backdrop` + `.modal` (centred, `--z-modal`), `.drawer`,
  `.user-menu`, `.toast`, `.tooltip`. The tooltip shows on hover **and on
  keyboard focus** (`.tooltip:focus-within`) — its content is the exact value,
  so pointer and keyboard must reach it alike. The drawer has two sizes. The **rail**
  (`.drawer`, `min(420px, 90vw)`) is the right-hand strip a form rides in. The
  **panel** (`.drawer` + `.drawer-panel`) is the same shell centred like the
  modal at `min(80vw, 1680px)` × `min(80vh, 1040px)` with the modal's shadow,
  for a payload an operator reads rather than a form they fill: a request's
  route trace and captured bodies. It carries no footer of its own, its body is
  the only scrolling region, and it releases `.form`'s 560px reading cap — that
  cap measures a column of inputs, and here it would only push the panel's extra
  width into the empty space beside the wrapped `pre` blocks the payload renders
  as. Below 768px it takes the whole viewport and
  drops its border and radius, which is the same step the sidebar takes there.
- **Lists** — `.feed` / `.feed-item` with a severity icon circle.
- **Logs** — `.logs-table` and the row vocabulary: `.log-row`, `.log-row-child`
  with the `.child-elbow` tree rail, `.td-gutter`, `.child-failover`,
  `.child-capability-fallback`, `.child-image-caption`, `.child-emulated-tool`,
  `.child-retry`, `.chip`/`.chip-healed`, `.pill`/`.pill-warm`, `.link-chip`,
  `.chain-summary`/`.chain-pill`, `.child-op-open`, `.raw-trace`, and the
  `.num-ti`/`.num-to`/`.num-cr` token triple.
- **App-owned components** — `.alias-editor` / `.alias-row` (the alias → model
  map), `.provider-type-card` (provider-type chooser), `.login-*` (auth page).

## 6. The neutral ramp

The ramp carries the canvas upward. Each step is the canvas plus light, tinted
cool (hue 250); nothing in the ramp is a hue of its own.

| Token | Value | sRGB |
|---|---|---|
| `--neutral-950` | `oklch(11% 0.011 250)` | `#030507` |
| `--neutral-900` | `oklch(14% 0.013 250)` | `#060a0e` |
| `--neutral-800` | `oklch(20% 0.018 250)` | `#10171e` |
| `--neutral-700` | `oklch(28% 0.015 250)` | `#232a30` |
| `--neutral-600` | `oklch(40% 0.018 250)` | `#414951` |
| `--neutral-500` | `oklch(56% 0.020 250)` | `#6c7680` |
| `--neutral-400` | `oklch(70% 0.015 250)` | `#989fa8` |
| `--neutral-300` | `oklch(81% 0.012 250)` | `#bbc2c9` |
| `--neutral-200` | `oklch(90% 0.008 250)` | `#dadee3` |

## 7. Mode, fonts, files

**Dark only.** `html{color-scheme: dark}` in the component layer — plus the
`color-scheme` meta tag in `webui/index.html` — pins the document, so the canvas,
form controls and scrollbars stay dark even when the browser is set to
`prefers-color-scheme: light`. A light appearance is not part of this system: it
would be a separate token layer with its own `--bg`, `--surface`, `--fg`,
`--muted`, `--border` and `--accent`, and inverting these values is explicitly
not the way to build one. Nothing in the console reads `prefers-color-scheme`.

Fonts live in one place, `webui/public/fonts/`, and are served from `/fonts/`:

| File | Family | License |
|---|---|---|
| `inter-latin-wght-normal.woff2` | Inter (variable 1–1000) | SIL OFL 1.1 |
| `plus-jakarta-sans-latin-wght-normal.woff2` | Plus Jakarta Sans (variable 200–800) | SIL OFL 1.1 |
| `jetbrains-mono-regular.woff2`, `jetbrains-mono-medium.woff2` | JetBrains Mono 400 / 500 | SIL OFL 1.1, `OFL-JetBrainsMono.txt` + `AUTHORS-JetBrainsMono.txt` |

All three families are declared with `@font-face` in `colors_and_type.css` and
render in the console: the display and body faces, and JetBrains Mono at 400 and
500 for the numbers, ids, keys and pills. The mono face is loaded from two files
rather than a variable one, so the two weights the console asks for are the two
that ship.

Switching a family on or off is a visible step: when JetBrains Mono was declared,
every number, key prefix and pill in the console re-laid out with its metrics,
which is why the committed Playwright screenshot baselines carry that change.
`webui/public/fonts/README.md` holds the two blocks and this note.

```
design/                    the system: rules, tokens, examples
  DESIGN.md                this file
  AGENTS.md                the short entry point
  colors_and_type.css      token source
  brand.json               machine-readable profile
  fonts/README.md          pointer to the single font source + licensing
  examples/                two pages: tokens.html, components.html
  assets/brand/wordmark.svg
webui/
  public/fonts/            the one font source, served at /fonts/
  src/styles/colors_and_type.css   runtime copy of the token source
  src/styles/kit.css               component layer
  src/styles/app.css               the two app-local additions
```

## 8. Do / Don't

**Do**

- Bind colours to tokens. Reach for a semantic role first (`--bg`, `--surface`,
  `--fg`, `--muted`, `--border`, `--accent`, `--accent-dim`), a ramp step only
  when a rule needs a finer nuance.
- Keep the page code token-only: `app.css` and every component under
  `webui/src` reference custom properties (or `currentColor`), never literals.
  The component layer is the one place allowed to compose a literal for a nuance
  no token covers — an alpha step, the input well — and it must stay inside an
  existing hue family, which is what the hue-window check in
  `e2e/design-rules.spec.ts` audits.
- Let a hairline do the separating, and keep the surfaces close: the console
  reads as one dark field with structure drawn on it.
- Use the display face for headings and page titles, the body face for
  everything else, the mono face for anything an operator compares down a column.
- Render a missing value as an em dash or an honest empty state.
- Keep every icon an inline SVG that inherits `currentColor`.

**Don't**

- Don't add a second chromatic colour. Status tones are the only other hues and
  they never decorate.
- Don't ship a light theme, and don't read `prefers-color-scheme: light`.
- Don't paint the canvas a mid grey (an `#111821`-like value) or pure black; the
  canvas is `oklch(11% 0.011 250)` and the surfaces above it are translucent.
- Don't override a token in a component rule, and don't let a literal creep past
  the component layer into `app.css` or a route component.
- Don't use emoji as icons; don't hot-link a remote asset; don't add a tracking
  script.
- Don't fabricate a metric. Numbers come from the store or they are absent.
- Don't use a left colour stripe on a card — the border turns accent on hover.
  The one permitted left edge is the sidebar's active-link indicator, because it
  marks navigation rather than a card.
- Don't put variable-length text on a flex-wrap line above other content: font
  metric drift between machines moves the wrap point and shifts everything below
  it by a line. Use block layout.
