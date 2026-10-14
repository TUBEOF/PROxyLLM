# Accessibility

The console is an operator tool: it is used with a keyboard, on a dark screen,
sometimes at 3 a.m. during an incident. These are the rules it holds to, and the
ones a change must not break.

## Focus

- The token layer carries the ring: `:focus-visible { outline: 2px solid var(--primary-500); outline-offset: 2px; }`.
  Nothing removes it; a rule that hides an outline must replace it with a
  visible one.
- Where the real control is hidden and a sibling is styled, the ring moves to the
  sibling: `.check input:focus-visible + .cbox`, `.toggle input:focus-visible + .tswitch`.
  The radio pills use `:has()` the same way.
- Rows that are buttons ring inside themselves so the ring is not clipped by the
  table: `.log-row-child:focus-visible` and `.child-op-open:focus-visible` use
  `outline-offset: -2px` with `--status-info`.
- `.provider-type-card:focus-visible` and every `.btn` take the accent ring.
- A row that opens a detail view is reachable by keyboard: `LogRow` sets
  `tabIndex={0}`, `role="button"` and handles `Enter` and `Space` (with
  `preventDefault` so the page does not scroll).

## Motion

- `prefers-reduced-motion: reduce` neutralises every animation and transition
  duration in the token layer, and the component layer switches the two
  decorative animations off outright (`status-ping` on the sidebar dot and the
  healthy status dot). The route spinner stops too, in `app.css`.
- Nothing depends on an animation to be understood: the ping marks a dot that is
  already green, the running dot sits inside a badge that already spells the
  status, and the spinner sits next to its own label.

## Contrast

Measured on the tokens, with the translucent surfaces composited over the canvas
(WCAG 2.1 ratio):

| Pair | Ratio | Note |
|---|---|---|
| `--fg` on `--bg` | 15.1:1 | body text |
| `--muted` on `--bg` | 11.4:1 | secondary text |
| `--neutral-400` on `--bg` | 7.6:1 | widget headers |
| `--neutral-500` on `--bg` | 4.4:1 | the dimmest text the system uses — AA for large text and UI, not for body copy |
| `--accent` on `--bg` | 11.0:1 | links, healthy status |
| `--status-down` / `--status-warn` / `--status-info` on `--bg` | 4.7:1 / 10.0:1 / 8.4:1 | status text |
| `--fg` on `--surface` (composited) | 14.9:1 | text on glass |
| `--muted` on `--surface` (composited) | 11.2:1 | |
| `--neutral-500` on `--surface` (composited) | 4.4:1 | |
| `--fg` on `--surface-elevated` (composited) | 14.9:1 | modals and drawers |
| `--status-down` on `--surface-elevated` (composited) | 4.6:1 | error text in a modal |
| `--neutral-900` on `--accent` | 10.7:1 | the primary button's dark text on the accent |

Consequences: `--neutral-500` is for labels, placeholders and meta, not for
sentences an operator must read. Body text is `--fg` or `--muted`. A new colour
pair must be measured before it ships — the token layer is the only place to add
one.

## Keyboard and semantics

- Interactive elements are `button`/`a`/`input`, never a clickable `div`. Rows
  that open something carry `role="button"` and a key handler.
- Dialogs: `role="dialog"` + `aria-modal="true"`, a labelled title, `Escape`
  closes, and the close control is a real button with `aria-label="Close"`.
- The account menu is `role="menu"` with `role="menuitem"` children, opens from a
  button carrying `aria-haspopup="menu"` and `aria-expanded`, and closes on
  outside click and `Escape`.
- Live regions: `role="status"` (+ `aria-live="polite"`) for progress, `role="alert"`
  for errors, so a failure is announced rather than only drawn. `LiveBadge` marks
  the live log stream the same way.
- Every form control has a `label` bound by `htmlFor`/`id`, or an `aria-label`
  when the visible label would be an icon.
- Icon-only controls and every decorative icon: `aria-hidden="true"` on the SVG
  and a name on the control.
- Tab order follows the reading order of the shell: sidebar → topbar → page. The
  collapse toggle is a real button with `aria-label` that changes with the state
  (`Expand sidebar` / `Collapse sidebar`).
- The mobile sidebar is a drawer, not a modal: it does not trap focus, and
  `Shell.tsx` closes it on navigation so it never covers the page under the
  pointer.

## Text and layout

- Text is real text: no text baked into images, no emoji as an icon or a label.
- The layout survives a 200% zoom and a 390px viewport: the sidebar becomes an
  off-canvas drawer, the canvas collapses to one column, and tables scroll
  horizontally inside `.table-scroll` instead of pushing the page sideways
  (`.table` keeps a 560px minimum).
- Column content is one line per cell, with the full value in the `title`
  attribute, so a long model id or account name cannot reflow the row.
