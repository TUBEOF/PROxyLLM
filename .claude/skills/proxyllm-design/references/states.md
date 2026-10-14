# States

Every interactive element answers to the pointer and the keyboard the same way,
and every asynchronous state says what it is. The rules below are the ones the
console actually renders.

## Pointer and keyboard

| State | Rule | Reads as |
|---|---|---|
| Hover — button | `.btn-primary:hover` swaps to `--gradient-primary-hover` (white overlay) + `--shadow-btn-hover`; `.btn-secondary:hover` lifts the border to `--neutral-600`; `.btn-ghost:hover` adds the accent glow on an 8% accent wash; `.btn-danger:hover` deepens to an 18% `--status-down` mix; `.btn-icon:hover`/`.btn-round:hover` raise the border, `.btn-round` takes the accent | the surface lightens, the accent glows |
| Hover — card | `.widget:hover` moves the border to `--neutral-600`; `.provider-type-card:hover` takes `--primary-500-alpha` and `--shadow-card-hover` | the card lifts, the accent outlines it |
| Hover — row | `.log-row:hover` and `.table tbody tr:hover` take `--row-hover` (and the row's actions appear: `.table tr:hover .row-actions` goes to opacity 1) | the row under the pointer is obvious |
| Hover — nav | `.sidebar-link:hover`, `.collapse-toggle:hover`, `.iconbtn:hover`, `.widget-head .menu:hover` take a translucent `--neutral-900` wash; `.user-menu-item:hover` takes `--accent-dim` | a quiet surface, not a colour change |
| Hover — sub-operation | `.log-row-child:hover` takes `oklch(20% 0.015 250 / 55%)` — deliberately stronger than the request row, because the row is a button | the step under the pointer separates from its parent |
| Active/selected | `.sidebar-link.active` takes the accent left indicator and a `--neutral-900` wash; `.tab.active` an accent underline; `.seg button.active` and `.rpill:has(input:checked)` an accent wash | where you are, and what is on |
| Focus | the token layer's `:focus-visible`: 2px `--accent` outline with a 2px offset. Controls that hide their input move the ring to the visible sibling (`.check input:focus-visible + .cbox`, `.toggle input:focus-visible + .tswitch`); the sub-operation row and its drawer entry ring inside the row (`outline-offset: -2px`) | always visible, never a hover substitute |
| Disabled | `.check input:disabled + .cbox` fades to 40% and `not-allowed`; `.check.disabled` and `.link-chip:disabled` go to `--muted`/`--neutral-500`; `.child-op-open:disabled` loses the pointer cursor | a control that cannot be used |
| Busy | a pending button keeps its label and is `disabled` while the request is in flight; forms mark the pending action | nothing moves the layout |

**Gap worth knowing:** a disabled `.btn` keeps its normal look — the console has
no `.btn:disabled` rule, so opacity, colour and cursor are unchanged (measured:
`opacity: 1`, the same colour as the enabled state, `cursor: pointer`). The
affordance today comes only from the `disabled` attribute. If a state needs to
*look* disabled, add the rule to the component layer
(`.btn:disabled{opacity:.45;cursor:not-allowed}`) — it is a visible change, so the
screenshot baselines move with it.

Also: the base `.btn` sets no colour of its own, so a `.btn` without a variant
falls back to the browser's button text colour (white under
`color-scheme: dark`). Always pair `.btn` with `-primary`, `-secondary`,
`-ghost`, `-danger` or `-icon`.

## Asynchronous and data states

| State | Markup | What the operator sees |
|---|---|---|
| Loading (route) | `.loading` + `.loading-spinner` + `.muted` label, `role="status"`, `aria-live="polite"`; the spinner is decorative (`aria-hidden`) and stops under `prefers-reduced-motion` | "Loading…" with a spinner; the label alone carries the state |
| Loading (data) | `.empty` with the title `Loading <thing>` and a one-sentence body naming what is being fetched | the page keeps its frame while data arrives |
| Empty | `.empty` + `.empty-ic` + `.empty-t` + `.empty-h`: a short title and one sentence that says what would put something there (`No client keys`, `No model traffic` / `Requests appear here once models serve traffic.`) | honest absence, never a fabricated row |
| Error (inline) | `.field.is-error` (border + 3px `--status-down` halo) with a `.error` or `.form-note` line; `role="alert"` so it is announced | what failed, next to the field that failed |
| Error (page/section) | `.badge.warn` / `.badge.down` in the section header, and a `.btn-ghost.btn-sm` to retry | the failure is visible without a modal |
| Running | `LogRow` renders the running shape: `.badge.info` with `.b-dot.b-dot-spin` and an ellipsis, every other cell an em dash, `data-running` on the row | a request that is still in flight, in the same table |
| Healed | `.chip.chip-healed` next to the status badge | the request failed over or retried and then succeeded |
| Sub-operation kinds | `.chip` + `.child-failover` / `.child-capability-fallback` / `.child-image-caption` / `.child-emulated-tool` / `.child-retry` | each step of a fallback walk is named and coloured by kind |

## The honest-data rule

A value the console does not have renders as `—` (em dash) or as an `.empty`
state. The em dash is a deliberate signal — "there is none" — and is never
replaced by a blank cell, which would read as "not loaded". The same rule drives
the UI copy: a loading state names what it fetches, an empty state says what
would fill it, and a metric is a number from the store or an em dash.
