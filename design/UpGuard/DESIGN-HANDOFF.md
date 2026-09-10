# DESIGN-HANDOFF.md — UpGuard Design System

A self-contained, distribution-ready design system for **dark, glassmorphic, monitoring-flavored** web surfaces. One accent (green = "up"), glass cards, Plus Jakarta Sans display + Inter body. Safe for a public repository: no third-party logos, no source-site assets, no tracking, no external URLs.

## What this package is

- **Token layer** — `colors_and_type.css` holds every color, type, spacing, radius, shadow, gradient, layout and motion token. Paste it verbatim as the first style layer.
- **Rules** — `DESIGN.md` is the human-readable authoritative expansion; `brand.json` is the machine-readable profile of the same system. `AGENTS.md` is the short entry point for any coding agent; `SKILL.md` is the Open Design skill wrapper.
- **Component kits** — `ui_kits/app/` (marketing: nav, hero, monitoring cards, footer) and `ui_kits/dashboard/` (tables, forms, controls, stats boards, charts, navigation, overlays). Copy shapes from here; don't hand-write glass cards or pill buttons.
- **Preview pages** — `preview/` holds 13 focused review pages (colors, type, spacing, radius/shadows, components, brand assets, applied UI, dashboard shell/tables/forms/stats). Each renders standalone with relative paths only.
- **Assets** — `assets/brand/wordmark.svg` (generic placeholder) and `assets/imagery/*.svg` (token-derived illustrations, no text, no third-party logos). **Fonts** — self-hosted woff2, relative `@font-face` paths.

## Repository layout

```
AGENTS.md              ← start here (any coding agent)
SKILL.md               ← Open Design skill entry
DESIGN.md              ← full design rules
brand.json             ← machine-readable system profile
colors_and_type.css    ← token layer (paste verbatim)
DESIGN-HANDOFF.md      ← this file
DESIGN-MANIFEST.json   ← machine-readable file inventory + tokens
fonts/                 ← inter + plus-jakarta-sans woff2
assets/brand/          ← placeholder wordmark SVG
assets/imagery/        ← token-derived SVG illustrations
preview/               ← 13 standalone review pages
ui_kits/app/           ← marketing component kit
ui_kits/dashboard/     ← admin-dashboard component kit
```

## Quick start for any project

1. Read `AGENTS.md`, then `DESIGN.md` and `brand.json`.
2. Paste or `@import` `colors_and_type.css` as the first style layer — never override its tokens.
3. Load the fonts from `fonts/` (the `@font-face` rules are already in `colors_and_type.css`; keep the woff2 files next to it or re-point the relative `url()` paths).
4. Copy component markup from the matching `ui_kits/*` kit and bind classes to the tokens.
5. Build dark, green, glass. No second accent, no raw hex outside tokens, no light mode by default (a light theme must be a separate, explicit token layer).

## Typography licensing

`fonts/` contains **Inter Variable** and **Plus Jakarta Sans** woff2 files. Both families are licensed under the **SIL Open Font License (OFL)** — free to use, embed, and redistribute in commercial and open-source projects, including this repository. No attribution required in product UI; keep the license notice with the files when redistributing.

## Portability notes

- Every HTML file in `preview/` and `ui_kits/` renders standalone: relative paths only, no external resources, no tracking scripts.
- Token names and values are identical across `DESIGN.md`, `brand.json`, `colors_and_type.css`, `ui_kits/`, and `preview/` — `DESIGN-MANIFEST.json` is the machine-checkable source of truth.
- Brand assets and demo copy are generic placeholders ("UpGuard", token-derived SVGs). Replace them with real brand assets before shipping a real product.
- The system is dark-first by design. A light theme is possible only as its own token layer — do not derive one by inverting these tokens.

## Verification

Check `DESIGN-MANIFEST.json` is well-formed JSON and that every file it lists exists (any JSON parser works, e.g. `python -m json.tool DESIGN-MANIFEST.json` or `jq . DESIGN-MANIFEST.json`).

Version: 1.0.0 · Tokens: 8 semantic + 7 primary + 11 neutral + 5 status + 5 chart · Mode: dark only