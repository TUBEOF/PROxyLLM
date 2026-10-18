# Components

The component layer is `webui/src/styles/kit.css` (plus the two app-local rules
in `app.css`). Every class below is rendered by the console today. Components are
composed from these classes; a page does not ship its own CSS.

Working markup: `design/examples/components.html`.

## Shell

| Class | Renders |
|---|---|
| `.appshell` | two-column grid: sidebar + content, `min-height: 100vh` |
| `.sidebar`, `.sidebar.collapsed` | 240px rail, 64px icon-only; sticky, full height, glass, 1px right border |
| `.sidebar-brand` | the accent dot + the wordmark; the row is 72px and is a link to `/` |
| `.sidebar-section` | a nav group label (`Monitor`, `Configuration`, `Access`) |
| `.sidebar-link`, `.sidebar-link.active` | a nav item: icon + label, accent left indicator when active |
| `.content` | the routed area under the topbar |
| `.topbar` | 72px sticky glass bar: collapse toggle, actions on the right |
| `.avatar`, `.iconbtn` | round 36px buttons; `.avatar` opens the user menu |
| `.user-menu`, `.user-menu-head`, `.user-menu-name`, `.user-menu-item`, `.user-menu-signout` | the account menu: identity, `Account settings`, `Sign out` (warn colour) |
| `.collapse-toggle` | the sidebar toggle inside the topbar |

## Page frame

| Class | Renders |
|---|---|
| `.workspace-head`, `.workspace-head h1`, `p.sub`, `.workspace-actions` | page title (display face, 26px), one-line subtitle, right-aligned actions |
| `.canvas`, `.w-1`, `.w-2`, `.w-3`, `.w-full` | the 12-column grid and its spans (3/6/9/12 columns) |
| `.widget`, `.widget-head`, `.widget-head .title`, `.widget-body` | the content unit: glass card, header with title and actions, body |
| `.filterbar`, `.seg`, `.select`, `.filter-input` | segmented filter, selects and text filters above a table |
| `.table-toolbar`, `.count`, `.pg-nav` | the row above a table: count on the left, page size and paging on the right |
| `.table-wrap`, `.table-scroll`, `.table` | the glass table container; the scroll wrapper keeps a wide table inside the page |

## Data display

| Class | Renders |
|---|---|
| `.kpi`, `.kpi-label`, `.kpi-value`, `.kpi-foot`, `.kpi-sub` | a metric tile: label, display-face number, foot with delta |
| `.delta`, `.delta.pos`, `.delta.neg`, `.delta.neu` | a signed delta badge |
| `.badge`, `.badge.up/.warn/.down/.paused/.info`, `.b-dot` | a status badge with a coloured dot; `.b-dot-spin` pulses while a request runs |
| `.status-dot`, `.mini`, `.status-grid` | dot indicator, tile grid of statuses |
| `.health`, `.health-ring`, `.health-num`, `.health-lab` | the health score ring |
| `.spark`, `.spark-area`, `.spark-line`, `.spark-pt` | a sparkline: filled area, accent line, last point |
| `.bars`, `.bar-row`, `.bar-lab`, `.bar-val`, `.bar-track`, `.bar-fill`, `.bar-fill.alt` | horizontal bars; `.alt` is the neutral comparison series |
| `.ring`, `.ring-track`, `.ring-seg` | a donut |
| `.usage-window`, `.usage-window-copy`, `.usage-window-label`, `.usage-left`, `.usage-ring`, `.usage-ring-track`, `.usage-ring-seg`, `.usage-ring-num`, `.usage-reset` | one Quota limit window: label + remaining value on the left, a centered ring with the remaining percent, the reset timer on the right (three grid columns, so the timer never wraps into the content) |
| `.heat`, `.heat-row`, `.heat-cell`, `.heat-legend` | a density heatmap |
| `.table thead th`, `.table tbody td`, `.table .mono`, `.table .num`, `.table .primary`, `.row-actions` | the data grid: sticky header, 13px/16px cells, mono right-aligned numbers, em dash for absent values, row actions revealed on hover |
| `.pagination`, `.selbar` | paging; the bulk-selection bar |
| `.empty`, `.empty-ic`, `.empty-t`, `.empty-h` | the honest empty state: an icon circle, a short title, one sentence |
| `.feed`, `.feed-item`, `.f-ic`, `.f-main`, `.f-title`, `.f-meta`, `.sev-*` | the event list behind the routing timeline |
| `.num` | mono with tabular figures |
| `.muted`, `.hint`, `.txt-muted`, `.heading`, `.sub` | text helpers |

## Forms and controls

| Class | Renders |
|---|---|
| `.form`, `.field`, `.field label`, `.hint`, `.error`, `.req` | the form column and a labelled field |
| `.input`, `.textarea`, `.select`, `.input-affix` | 40px controls on a darker well with a 1px border |
| `.field.is-error`, `.field.is-valid` | error and valid states (border + halo + inline message) |
| `.fieldset`, `.form-row`, `.form-actions` | grouping, two-column row, the action row |
| `.form-note` | the error line a form shows instead of an alert box (status-down, `.fs-sm`) |
| `.check`, `.cbox`, `.toggle`, `.tswitch`, `.radio`, `.rdot`, `.rpills`, `.rpill` | checkbox, switch, radio and radio pills — each hides a real input and styles the sibling |
| `.btn`, `.btn-primary`, `.btn-secondary`, `.btn-ghost`, `.btn-danger`, `.btn-sm`, `.btn-icon`, `.btn-round`, `.btn-group` | buttons: accent gradient, surface outline, accent outline, destructive, small, icon-only, round, grouped |
| `.tabs`, `.tab`, `.breadcrumb` | tabs and breadcrumb |

## Overlays

| Class | Renders |
|---|---|
| `.modal-backdrop`, `.modal`, `.modal-head`, `.modal-body`, `.modal-foot` | the modal: fixed backdrop (60% black + 4px blur, z 200) with the centred dialog on top (max-width 480px, z 210). The SPA renders the pair as siblings |
| `.drawer-backdrop`, `.drawer`, `.drawer-head`, `.drawer-body`, `.drawer-foot` | the detail overlay, two sizes: the right-hand rail `min(420px, 90vw)` for a form, and — as `.drawer` + `.drawer-panel` — a popup centred at `min(80vw, 1680px)` × `min(80vh, 1040px)` for a payload. z 160, backdrop z 150; full-viewport below 768px |
| `.toast-wrap`, `.toast`, `.t-ic`, `.t-title`, `.t-msg`, `.t-close` | status-coloured notifications |
| `.tooltip`, `.tooltip .tt` | a label above its trigger, on hover and keyboard focus |
| `.overlay-host` | the positioning context for an inline overlay |

## The request log

`LogRow.tsx` renders one line per request; `RouteTrace.tsx` renders the trace.
Both compose the kit's vocabulary:

| Class | Renders |
|---|---|
| `.logs-table` | the request log table |
| `.log-row` | a request row: status, model, provider, protocol, account, key, tokens, TPS, duration, time |
| `.log-row-child`, `.td-gutter`, `.gutter-spacer`, `.child-elbow` | a sub-operation row and the tree that ties it to its parent: a continuous rail down the gutter (`--rail`) with an elbow into the row |
| `.child-failover`, `.child-capability-fallback`, `.child-image-caption`, `.child-emulated-tool`, `.child-retry` | the op's kind chip (`childCls()` in `RouteTrace.tsx`) |
| `.chip`, `.chip-healed` | `healed` marks a request that failed over or retried and then succeeded |
| `.pill`, `.pill-warm` | provider and protocol badges; `.pill-warm` for the protocol |
| `.link-chip` | an in-row action that is a button, not decoration |
| `.chain-summary`, `.chain-pill`, `.chain-pill.serving` | the attempted provider chain; the last pill is the serving one |
| `.child-ops`, `.child-op-list`, `.child-op-open`, `.child-op-detail` | the drawer's list of a request's sub-operations, each opening its own detail |
| `.raw-trace` | the `<details>` holding the exact stored JSON |
| `.col-account`, `.col-key`, `.col-model`, `.col-tokens`, `.col-time` | the ellipsising column caps; one line per cell is what keeps the rail's elbow aligned |
| `.num-ti`, `.num-to`, `.num-cr` | the token triple: input (resting text colour), output (accent), cache-read (info) |

## App-owned components

| Component | Classes | Notes |
|---|---|---|
| `LogRow.tsx` | `.log-row`, `.log-row-child`, `.num-ti/.num-to/.num-cr`, `.col-*` | pure render; all derivation arrives via props. `tokensCell`, `tpsOf` and `keyDisplay` are shared so a row and its drawer never disagree |
| `RouteTrace.tsx` | `.chain-*`, `.feed*`, `.raw-trace`, `childCls()`/`childTitle()` | the one place that reads the `route_trace` blob; an unrecognised shape degrades to the raw view |
| `AliasEditor.tsx` | `.alias-editor`, `.alias-row`, `.alias-input`, `.free-pick`, `.alias-row-new` | the alias → model map: a row per alias (input + model picker + remove) plus the add row. `role="group"`, per-row `aria-label`s |
| `ProviderType` card | `.provider-type-card`, `.type-head`, `.title`, `.type-desc`, `.type-auth`, `.type-count` | the provider-type chooser: glass card, hover lift, description block, auth hint, account count. Block layout throughout |
| `UserMenu.tsx` | `.avatar`, `.user-menu*` | portalled to `document.body` so the topbar's `backdrop-filter` cannot become the containing block for the modal |
| `Overview` / `Quota` pages | `.kpi`, `.bars`, `.ring`, `.spark`, `.widget` | the stats boards |
| `Login` / `Loading` | `.login-wrap`, `.login-card`, `.login-error`, `.loading`, `.loading-spinner` | the only two rules `app.css` contributes |
