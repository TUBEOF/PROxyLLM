# Layout

The app shell is defined in `kit.css` under `APP SHELL` and `WIDGET CANVAS`. A
page fills the content area; it does not set its own page margins or widths.

## Shell

```
.appshell        grid-template-columns: auto 1fr; min-height: 100vh
  .sidebar       240px (collapsed 64px), sticky, height 100vh, glass, 1px right border
     .sidebar-brand    72px (--nav-height): accent dot + wordmark
     .sidebar-nav      12px padding, 2px between links, scrolls on its own
        .sidebar-section   group label, 14px 12px 6px
        .sidebar-link      icon 20px + label, 10px 12px, 12px gap
  .content       flex column, min-width 0 (the table may be wider than the viewport)
     .topbar      72px, sticky, glass, padding 0 24px, 16px gaps, z 50
     <route>      the page
        .workspace-head   page title + subtitle + actions
        .filterbar        20px bottom padding
        .canvas           12-column grid, 16px gap, padding 0 24px 24px
```

Both the sidebar brand row and the topbar take their height from
`--nav-height` (72px), so their hairlines line up across the shell.

## The widget canvas

`.canvas` is `grid-template-columns: repeat(12, 1fr); gap: 16px`. A widget takes

| Class | Width |
|---|---|
| `.w-1` | 3 of 12 columns |
| `.w-2` | 6 of 12 |
| `.w-3` | 9 of 12 |
| `.w-full` | 12 of 12 |

Widgets are `.widget` boxes (glass, 12px radius, 1px border); a page is normally
a stack of `.w-full` widgets, and `w-1`/`w-2`/`w-3` sit side by side where two
measures belong together. Nested layout inside a widget uses flex/grid with the
same rhythm, not new margins.

## Breakpoints

| Width | What changes |
|---|---|
| ≤ 1100px | `.w-1` becomes 6 columns, `.w-2` and `.w-3` become full width |
| ≤ 768px | the sidebar leaves the flow and becomes an off-canvas drawer (fixed, `translateX(-100%)`, z 120, opened by the collapse toggle, which then shows the full 240px sidebar); `.appshell` becomes one column; the canvas collapses to one column with a 12px gap; every `.w-*` spans one; `.form-row` stacks; `.table` keeps a 560px minimum width and scrolls horizontally inside `.table-scroll` |

The drawer's open/closed state is `.sidebar.collapsed` — on mobile the class
means "the drawer is out", on desktop "the rail is icon-only". `Shell.tsx` resets
it on navigation so the drawer never covers the page after a route change.

## Density — the numbers that actually recur

The base unit is 4px; the component layer composes its multiples directly rather
than through a spacing scale.

| Measure | Value |
|---|---|
| Page gutter | 24px (`.topbar` padding, `.canvas` side padding) |
| Widget gap | 16px |
| Widget | `.widget-head` 14px 18px, `.widget-body` 18px |
| Table cell | `.table tbody td` 13px 16px |
| Table container | `.table-wrap` 12px radius, hairline between rows |
| Sidebar | `.sidebar-nav` 12px, `.sidebar-link` 10px 12px, 12px icon gap |
| Form | `.field` 6px gap, `.form` 20px gap, controls 40px tall |
| Feed row | 11px 0, 12px gap, 30px icon circle |
| Status grid | `minmax(54px, 1fr)`, 8px gap |
| Section label | 14px 12px 6px |

Numbers in the shell are aligned by column, not by whitespace: the request log
keeps one line per row (the `.col-*` caps ellipsise instead of wrapping), because
a wrapped cell breaks both the scan line and the tree rail's elbow alignment.

## Two rules that have bitten before

- **Never put variable-length text in a flex-wrap line above other content.**
  Font-metric drift between machines moves the wrap point and shifts everything
  below it by one line. Use block layout.
- **A style refactor must not move a pixel.** The screenshot baselines are
  rendered in CI; run the assertion suite locally and keep the layout identical
  (`references/accessibility.md` has the commands).
