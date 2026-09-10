---
name: upguard-design-system
description: |
  Dark, glassmorphic, green-as-status design system for monitoring and observability
  product surfaces. Use when the brief asks for a dark theme, glass cards, a monitoring /
  observability / status dashboard, or an uptime SaaS feel with a single operational
  green accent. Provides tokens, fonts, SVG assets, applied UI kits, and preview pages.
user-invocable: true
od:
  mode: design-system
  category: design-systems
  surface: web
  design_system:
    requires: false
---

# UpGuard Design System

A self-contained design system for **dark, technical, monitoring-flavored** web surfaces. One accent (green = "up"), glassmorphic cards, Inter body + Plus Jakarta Sans display. Fully portable: every rule is derivable from `DESIGN.md`, `brand.json`, and `colors_and_type.css` alone.

**Machine-readable system reference:** `brand.json` — token roles, voice posture, component rules, implementation workflow, and anti-patterns. Read it alongside this file.

## What is inside

| Path | Purpose |
|---|---|
| `DESIGN.md` | Authoritative rules: context, color, type, spacing, layout, components, motion, voice, anti-patterns, dashboard kit. |
| `brand.json` | Machine-readable design-system profile: tokens with roles, voice posture, component guidance, reuse workflow. |
| `colors_and_type.css` | Reusable token layer: six semantic tokens, green primary ramp, dark neutral scale, type stacks, spacing, radius, shadows, gradients. Paste verbatim. |
| `fonts/` | Self-hosted woff2 — Inter Variable + Plus Jakarta Sans. Both are SIL OFL licensed. |
| `assets/brand/` | Generic wordmark placeholder SVG. |
| `assets/imagery/` | Token-derived SVG illustrations (hero + feature scenes, no text, no third-party logos). |
| `preview/` | 13 focused review pages (colors, typography, spacing, radius/shadows, components, brand assets, applied UI, dashboard shell/tables/forms/stats). |
| `ui_kits/app/` | Applied interface kit — nav, hero, monitoring cards, footer — on these tokens. |
| `ui_kits/dashboard/` | Admin-dashboard component kit — tables, forms, controls, stats boards/charts, navigation, overlays. Status/chart tokens in `colors_and_type.css` are derived (not a second accent). See `DESIGN.md` §10. |
| `AGENTS.md` | Agent-agnostic quick entry point — the short, imperative rules any coding agent needs. |

## When to use

- Building a monitoring / observability / SaaS-status product page or dashboard.
- A brief asks for "dark theme", "glass cards", "operational feel", or "like an uptime-monitoring SaaS".
- You need a calm dark canvas with exactly one signal color (green) for CTAs and status.
- Do NOT use for light/cream, multi-accent, or editorial briefs — this is a single-accent dark operational system.

## How to use

1. **Read `AGENTS.md`** for the short version, then `DESIGN.md` and `brand.json` for the full rules and token roles.
2. **Paste `colors_and_type.css` verbatim** into the first `<style>` block (or `@import` it) and bind its tokens. Never invent hex outside this palette.
3. **Load the self-hosted fonts** from `fonts/` — add the `@font-face` declarations from `colors_and_type.css`. Do not fall back to system fonts when the woff2 files are present.
4. **Copy component shapes** from `ui_kits/app/` (nav, hero, monitoring cards, footer) or `ui_kits/dashboard/` (tables, forms, stats) and the `preview/` pages. Don't hand-write the glass card or the primary pill button from scratch.
5. **Stay on token roles.** Use the semantic tokens (`--bg`, `--surface`, `--surface-elevated`, `--fg`, `--muted`, `--border`, `--accent`) as the public API; reach for `--primary-*` / `--neutral-*` only for finer nuance.
6. **No light mode by default.** If the brief explicitly asks for a light theme, build a separate token layer; do not invert dark tokens.

## Design-system highlights

- **One accent, green = "up".** `--accent` `#3bd671`. CTAs, link hovers, status glows, logo dot. No second chromatic accent; the primary button gradient is green→lighter-green only.
- **Dark charcoal canvas.** `--bg` `#111821`, `color-scheme: dark`. Dark-first by design.
- **Glass surfaces.** Cards = translucent surface + `backdrop-filter: blur(10px)` + 1px `--border` → green on hover. Elevated surfaces (`--surface-elevated`) for modals/drawers/toasts. No left-stripe accent.
- **Display ≠ body.** Plus Jakarta Sans for headings/hero/footer; Inter for body/UI/buttons.
- **Icons = inline SVG** filled `currentColor`/`--accent`. No emoji icons.
- **Restrained motion.** Card hover lifts `-2px` + green shadow on `0.4s`; link/color on `0.15s`; buttons `0.3s`. `prefers-reduced-motion` honored (bound in `colors_and_type.css`).
- **No fabricated metrics.** Concrete numbers only; missing values → honestly-labelled placeholders.
- **Dashboard kit is on the same system.** `ui_kits/dashboard/` extends to tables/forms/controls/stats with derived status colours (`--status-warn/down/paused/info`) and chart tokens (`--chart-line/area/grid`) — all oklch, never a second brand accent. Charts fill, never outline; the sidebar active bar is the only permitted left accent (navigation, not a card). See `DESIGN.md` §10.

## Core rules (non-negotiables)

- Dark canvas; never light/cream default.
- One green accent only; never purple/blue/indigo gradients.
- Glass cards with a full 1px border that turns green on hover; never a left color-stripe.
- Plus Jakarta Sans display, Inter body; never Inter as a display face.
- Inline SVG icons; never emoji.
- Local assets only; never hot-link remote image URLs.
- No tracking or analytics scripts.
- No outline-only charts; encode with fills.
- No fabricated metrics; honest placeholders only.

## Voice posture

- Direct and benefit-first — every message answers "what does this do for me?"
- Confident but plain — no hype, no exclamation overload.
- Operational and calm — the product is reliable; the voice reflects that.
- Technical enough for DevOps/SRE audiences, but never gate-kept by jargon.

Use short imperative CTAs, lead feature sentences with the verb, keep numbers concrete, and stay in sentence case for headings/nav. See `DESIGN.md` §8 and `brand.json` `voice`.

## Files to read first

- `AGENTS.md` — the short, agent-agnostic rules.
- `DESIGN.md` — all rules, per section.
- `brand.json` — token roles, component rules, workflow.
- `colors_and_type.css` — the token layer to paste.
- `ui_kits/app/index.html` — a working applied marketing example.
- `ui_kits/dashboard/index.html` — a working applied dashboard example.