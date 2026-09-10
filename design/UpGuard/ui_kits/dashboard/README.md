# Admin Dashboard Kit — UpGuard

An applied component kit for dark admin / monitoring dashboards, built on the design-system tokens (`../../colors_and_type.css`) via `kit.css`. It is the dashboard companion to the marketing kit in `ui_kits/app/` — same dark charcoal canvas, same single green accent, same glass surfaces — extended with the operational UI an admin surface needs: tables, forms, checkboxes, stats boards, charts, navigation and overlays.

This is a **reusable component package**, not a finished product: copy the files, re-point paths, and the kit stays on-system. The placeholder name "UpGuard" should be replaced with the actual product name before public deploy.

## Files

| Path | What |
|---|---|
| `index.html` | Launcher + live "Operations Overview" example (app shell, sidebar, topbar, KPI widgets, charts, alarm feed) plus links to every component file. Start here. |
| `kit.css` | Component layer — `@import`s the tokens, declares `@font-face` for the self-hosted fonts, and defines every dashboard component class. Paste after the token layer; never override tokens here. |
| `components/table.html` | Data grid: sortable headers, row hover, status badges, bulk selection + selection bar, pagination, empty state. |
| `components/forms.html` | Fields, inputs, select, textarea, input affix, validation states, fieldset. |
| `components/controls.html` | Checkbox (SVG checkmark), indeterminate, toggle switch, radio, radio pills. |
| `components/buttons.html` | Primary, secondary, ghost, danger, small, icon-only, button group. |
| `components/status.html` | Status badges, status dots, health-score ring, status grid. |
| `components/charts.html` | KPI tile + sparkline, ring/donut, horizontal bars, heatmap — fills not outlines. |
| `components/navigation.html` | App shell, collapsible sidebar, topbar + search, tabs, breadcrumb. |
| `components/overlays.html` | Modal, drawer, toast, tooltip. |
| `components/widget.html` | Widget tile, 12-column sizes, empty state. |

## Components

The kit covers the full admin-dashboard component surface: **App** shell + sidebar nav, **Buttons** (primary/secondary/ghost/danger/icon/group), **Tables** (data grid + bulk + pagination), **Forms** (fields + validation), **Checkbox/Toggle/Radio** controls, **Cards** (widget tiles), **Sidebar** navigation, plus charts, status boards and overlays. Each maps cleanly to a React component — drop the markup into `components/*.jsx`, bind `className` to the same kit classes, and consume the tokens from a shared `colors_and_type.css` import. The `App` shell composes the Sidebar, topbar, and the widget canvas the same way `index.html` does.

## Component map (token → class)

- **App shell** → `.appshell` / `.sidebar` / `.sidebar-link` / `.topbar` / `.search` / `.avatar`
- **Widgets** → `.canvas` / `.w-1` `.w-2` `.w-3` `.w-full` / `.widget` / `.widget-head` / `.widget-body` / `.empty`
- **KPI / stats** → `.kpi` / `.kpi-value` / `.delta` / `.spark` / `.ring` / `.bars` / `.heat`
- **Status** → `.badge` (`.up/.warn/.down/.paused/.info`) / `.status-dot` / `.health` / `.status-grid` / `.mini`
- **Tables** → `.table-wrap` / `.table` / `.selbar` / `.pagination` / `.row-actions`
- **Forms** → `.field` / `.input` / `.textarea` / `.input-affix` / `.fieldset` / `.is-error` / `.is-valid`
- **Controls** → `.check` / `.cbox` / `.toggle` / `.tswitch` / `.radio` / `.rdot` / `.rpills` / `.rpill`
- **Buttons** → `.btn` / `.btn-primary` / `.btn-secondary` / `.btn-ghost` / `.btn-danger` / `.btn-icon` / `.btn-group`
- **Overlays** → `.modal` / `.drawer` / `.toast` / `.tooltip`
- **Nav** → `.tabs` / `.tab` / `.breadcrumb` / `.filterbar` / `.seg`

## Usage workflow

1. Copy `kit.css` and the `../../fonts/` folder into your project, or re-point the `@import` and `@font-face` `url()` paths at the top of `kit.css`.
2. Link `colors_and_type.css` **then** `kit.css` in your HTML (tokens first; kit consumes them).
3. Use the class names from `kit.css` directly (`.appshell`, `.widget`, `.table`, `.kpi`, `.badge`, `.btn`, `.field`, `.check`, …) — they are bound to the tokens, so they stay on-system.
4. For a full dashboard, copy the structure from `index.html`; for single components, open the matching `components/*.html`.

## Design notes

- **Tool UI, not marketing.** Higher density (16px gap, compact widget padding), restrained hover (border shift, no strong glow). The green accent stays the only chromatic accent.
- **Stats boards fit the system.** KPI tiles reuse the glass card; charts use the single green main series with neutral comparison tones — fills, never bare outlines. Demo values are honestly labelled; missing data → empty state.
- **Status colours are derived.** `--status-up` is the brand green; warn/down/paused/info are derived monitoring semantics, marked as derived. Never a second brand accent.
- **Sidebar active indicator** is a green left bar — the intentional, documented exception to the "no left-stripe on cards" rule, because it signals navigation, not a card.
- **One green accent.** `--accent #3bd671` for operational/positive; `--status-down` red only for destructive/critical states. No rainbow, no purple/blue gradients.
- **Display ≠ body.** Plus Jakarta Sans for headings; Inter for body/UI; JetBrains Mono for numbers/IDs.
- **Icons = inline SVG** filled `currentColor`/`#3BD671`; no emoji.
- **Motion** honors `prefers-reduced-motion` (status-dot ping, drag handle, toast transitions neutralize).

## Source basis

Built entirely on this design system: tokens from `../../colors_and_type.css`, component rules from `../../DESIGN.md` and `../../brand.json`. Dashboard status/chart tokens are derived from the green ramp and documented as such in `DESIGN.md` §10.