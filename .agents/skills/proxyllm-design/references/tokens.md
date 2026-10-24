# Tokens

`design/colors_and_type.css` is the source; `webui/src/styles/colors_and_type.css`
is the runtime copy the app loads and must stay byte-identical (a Go test in
`webui/` fails if they drift). Values are authored in `oklch`; the sRGB figures
below are what a browser composites, and they are never a value to copy.

## The public API — seven roles

Use these first. They are the vocabulary a page should be readable in.

| Token | Value | sRGB | Use it for |
|---|---|---|---|
| `--bg` | `oklch(11% 0.011 250)` | `#030507` | the canvas. Near-black with a faint cool tint |
| `--surface` | `oklch(13% 0.012 250 / 80%)` | — | glass surfaces: cards, widgets, tables, pills, the sidebar — with `backdrop-filter: blur(var(--blur-card))` |
| `--surface-elevated` | `oklch(13% 0.013 250 / 96%)` | — | anything floating over the page: modal, drawer, user menu, toast |
| `--fg` | `oklch(90% 0.008 250)` | `#dadee3` | primary text and high-emphasis UI |
| `--muted` | `oklch(81% 0.012 250)` | `#bbc2c9` | secondary text: nav, captions, help text, table meta |
| `--border` | `oklch(28% 0.015 250)` | `#232a30` | hairlines, card borders, dividers |
| `--accent` | `oklch(78% 0.20 144)` | `#56d75b` | action, focus, links, healthy status, the sidebar dot |
| `--accent-dim` | `oklch(78% 0.20 144 / 12%)` | — | soft accent fills: chips, icon circles, menu hover |

## The accent

| Token | Value | sRGB | Use it for |
|---|---|---|---|
| `--primary-500` | `oklch(78% 0.20 144)` | `#56d75b` | the accent itself (`--accent` points here) |
| `--primary-400` | `oklch(80% 0.22 145)` | `#45e159` | the lighter end of the primary-button gradient, nothing else |
| `--primary-500-alpha` | `oklch(78% 0.20 144 / 35%)` | — | the border of an interactive card on hover |

Two steps, one hue. A third green is not an option: a quieter accent is an alpha
of the accent (`--accent-dim`), and the tree rail is the accent at reduced alpha.

## The neutral ramp

| Token | Value | sRGB | Use it for |
|---|---|---|---|
| `--neutral-950` | `oklch(11% 0.011 250)` | `#030507` | the canvas |
| `--neutral-900` | `oklch(14% 0.013 250)` | `#060a0e` | row surfaces; inputs use it at 60% alpha |
| `--neutral-800` | `oklch(20% 0.018 250)` | `#10171e` | dropdown and card borders, ring track |
| `--neutral-700` | `oklch(28% 0.015 250)` | `#232a30` | borders and dividers (= `--border`) |
| `--neutral-600` | `oklch(40% 0.018 250)` | `#414951` | hover borders, button borders, separators |
| `--neutral-500` | `oklch(56% 0.020 250)` | `#6c7680` | tertiary text, placeholders, section labels |
| `--neutral-400` | `oklch(70% 0.015 250)` | `#989fa8` | widget headers, disabled text, comparison series |
| `--neutral-300` | `oklch(81% 0.012 250)` | `#bbc2c9` | secondary text (= `--muted`) |
| `--neutral-200` | `oklch(90% 0.008 250)` | `#dadee3` | the lightest text step (= `--fg`) |

Everything in the ramp is the canvas plus light at hue 250. No step is a hue of
its own, and there is no light-mode ramp to invert.

## Status

| Token | Value | sRGB | Means |
|---|---|---|---|
| `--status-up` | `var(--primary-500)` | `#56d75b` | 2xx, healthy, online |
| `--status-warn` | `oklch(78% 0.15 75)` | `#efa831` | degraded, retried, failed over, 4xx |
| `--status-down` | `oklch(60% 0.20 25)` | `#de3b3d` | 5xx, failed, destructive |
| `--status-paused` | `oklch(62% 0.015 250)` | `#80878f` | paused, inactive, unknown |
| `--status-info` | `oklch(72% 0.09 230)` | `#65afd2` | informational: sub-operations, capability fallbacks, image captions |

Status colours carry state. They are never a highlight, a hover or a decoration.
A 4xx is warn, a 5xx is down — see `LogRow.tsx` for the one place that maps them.

## Data surfaces

`--chart-line` (accent) is the single series; `--chart-area` its fill;
`--chart-2` (`--neutral-400`) the one neutral comparison series. Charts are
filled, never outlines. `--metric-pos`/`--metric-neg` colour a delta badge,
`--row-hover` a table row, `--widget-head` a widget header, `--bg-elev` the
achromatic wash that marks a row being moved.

## App-surface tokens

Three tokens exist because a rule of this app needed a value no role covered:

| Token | Value | Why it exists |
|---|---|---|
| `--rail` | `oklch(70% 0.17 143 / 72%)` | the request-log tree rail. `--accent-dim` (12% alpha) vanished against the dark row surface, so the parent → sub-operation lines needed the accent at higher alpha and a quieter lightness |
| `--text` | `var(--neutral-300)` | the resting colour of a number inside a data cell. It equals the table's own cell colour, named so the token column can point at it instead of at a ramp step |
| `--bg-elev` | `rgba(127, 127, 127, 0.12)` | the achromatic wash that marks a reordered row. Deliberately grey: a moved row is not a status |

## Type, space, shape, motion

| Group | Tokens |
|---|---|
| Families | `--font-display` (Plus Jakarta Sans), `--font-body` (Inter), `--font-mono` (JetBrains Mono) |
| Sizes | `--fs-xs` 12px, `--fs-sm` 14px, `--fs-base` 16px, `--font-size-body` 16px, `--line-height-body` 1.5 |
| Weights | `--fw-regular` 400, `--fw-medium` 500, `--fw-semibold` 600, `--fw-bold` 700 |
| Radius | `--radius-sm` 6px, `--radius-md` 8px, `--radius-lg` 12px, `--radius-full` 9999px |
| Shadow | `--shadow-glass`, `--shadow-card-hover`, `--shadow-btn-primary`, `--shadow-btn-hover` |
| Gradient | `--gradient-primary`, `--gradient-primary-hover`, `--gradient-nav-glass` |
| Motion | `--transition-base` 0.15s, `--transition-button` 0.3s |
| Blur | `--blur-glass` 2.5px, `--blur-card` 10px |
| Layout | `--nav-height` 72px |
| Stacking | `--z-nav` 50, `--z-modal` 210 |

There is no spacing scale: the component layer composes the 4px rhythm directly
(see `references/layout.md`), and a new spacing token is not the way to add one.

## The raw-value rule

- Page code (`app.css`, `webui/src/**`) uses tokens or `currentColor`. Today
  exactly one literal survives there, the `--bg-elev` fallback in `PrioList.tsx`.
- The component layer may compose a literal for a nuance no token covers (an
  alpha step, the input well), but it must stay inside a hue family the design
  rules already allow. `e2e/design-rules.spec.ts` enforces exactly that: every
  rendered colour must sit in the neutral-blue, accent-green, status-red,
  status-amber or status-blue window, or be achromatic.
- The token layer itself is the only place a value is defined.

## Fonts

`webui/public/fonts/` is the one font source, served at `/fonts/`. All three
families are declared with `@font-face` in the token file and render: Inter and
Plus Jakarta Sans, and JetBrains Mono at 400 and 500 — the two weights the
console's numbers, ids, keys and pills ask for, shipped as two files.
`OFL-JetBrainsMono.txt` carries that family's SIL OFL licence text. Switching a
family on or off re-lays out everything it renders and therefore moves the
screenshot baselines; `webui/public/fonts/README.md` holds the blocks.
