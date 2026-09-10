# UpGuard — Design System

> A dark, glassmorphic monitoring-product identity inspired by an uptime-monitoring SaaS homepage.
> One sentence: a deep charcoal canvas, green-as-status accent, Inter body with Plus Jakarta Sans display, glass cards with green-glow hover.
>
> **System quick reference:** `brand.json` contains the token roles, voice posture, component rules, and implementation workflow in machine-readable form. `DESIGN.md` is the human-readable authoritative expansion.

---

## 1. Product Context & Visual Atmosphere

### Product context
Website/uptime monitoring SaaS — the homepage sells free-to-start monitoring (website, keyword, ping, port, cron job), with an enterprise tier. The visual job is to feel calm, trustworthy, and "operational": dark, technical, with green as the single signal color that means *things are up*.

**Mood:** dark, technical, confident, slightly premium. Glassmorphic surfaces (blurred translucent panels) over a near-black charcoal background. A single bright green accent does all the lifting — CTAs, link hovers, status glows, logo dot. No purple, no multi-hue gradients, no rainbow status colors. The green is the brand.

**Mode:** the system ships `color-scheme: dark` with a `#111821` page background. There is **no light theme** in this system. It is therefore dark-first; a light mode would be a separate, out-of-system token layer and must not be implied as default.

**Authoritative values:** the dark canvas (`#111821`), the green accent ramp (`--primary-500:#3bd671`), and the `color-scheme: dark` + green intent are bound in `colors_and_type.css` and profiled in `brand.json` — those two files are the single source of truth.

---

## 2. Color

All values are bound in `colors_and_type.css` and mirrored in `brand.json` — those files are the source of truth. OKLch equivalents included inline.

### Primary (green) — the one accent
| Token | Hex | Role |
|---|---|---|
| `--primary-300` | `#86efac` | light hover/active text |
| `--primary-400` | `#3fe077` | gradient stop (primary button lighter end) |
| `--primary-500` | `#3bd671` | **brand accent** — CTAs, links, logo dot, glows, status-up |
| `--primary-600` | `#34c164` | pressed / active state |
| `--primary-700` | `#158140` | deep green |
| `--primary-800` | `#1c6636` | darkest green |
| `--primary-500-alpha` | `#3bd67159` | card-hover green shadow / glow |

### Neutral scale (dark theme base)
| Token | Hex | Role |
|---|---|---|
| `--neutral-50` | `#fafbfc` | lightest; rare inverse contexts |
| `--neutral-100` | `#f0f3f7` | light / inverse surfaces |
| `--neutral-200` | `#e2e8f0` | footer links, tagline |
| `--neutral-300` | `#cbd4e0` | nav links, secondary text |
| `--neutral-400` | `#a0adc0` | tertiary / disabled / chart comparison series |
| `--neutral-500` | `#72849e` | footer separator, meta |
| `--neutral-600` | `#4a5768` | button borders, ghost button border |
| `--neutral-700` | `#2d3540` | **borders / dividers** |
| `--neutral-800` | `#1c2634` | dropdown / card borders |
| `--neutral-900` | `#131b25` | glass card surface base |
| `--neutral-950` | `#111821` | **page background** |

### Semantic roles (six bound public tokens)
Use these as the public API. Fall back to `--primary-*` / `--neutral-*` only when a finer nuance is needed.

- `--bg` `#111821` — page / canvas background
- `--surface` `#131b25cc` — translucent glass cards (`backdrop-filter: blur(10px)`)
- `--surface-elevated` `#131b25f5` — more opaque surfaces for modals / drawers / toasts where extra contrast is needed
- `--fg` `#e2e8f0` — primary text / high-emphasis UI
- `--muted` `#cbd4e0` — secondary / nav / help text
- `--border` `#2d3540` — hairlines, card borders, dividers
- `--accent` `#3bd671` — CTAs, links, status-up
- `--accent-dim` `#3bd6711f` (12% alpha) — soft accent fill: icon circles, round-button hovers, chip backgrounds

### Status
- `--status-up` `#3bd671` (= `--accent`) — operational / healthy (measured)
- `--status-warn` `oklch(78% 0.15 75)` — degraded / partial / soon-expiring (derived)
- `--status-down` `oklch(60% 0.20 25)` — down / critical / failed (derived)
- `--status-paused` `oklch(62% 0.015 250)` — paused / inactive (derived)
- `--status-info` `oklch(72% 0.09 230)` — info / neutral hint (derived)

> Note: the source clone homepage shows only the green status accent; warn/down/paused/info are derived from standard monitoring semantics, not directly on the cloned page. They are marked as derived everywhere they appear.

### Gradients (measured)
- Primary button: `linear-gradient(135deg, #3bd671 0%, #3fe077 100%)`
- Nav glass: `linear-gradient(#111821cc 0%, #131b25cc 100%)` + `backdrop-filter: blur(2.5px)`
- Footer: `linear-gradient(#3bd6710d 0%, #11182100 47.63%), #111821`
- Horizontal divider: `linear-gradient(to right, transparent, #2d3540 25%, #2d3540 75%, transparent)`

---

## 3. Typography

**Type roles are bound in `colors_and_type.css`** (`--font-display`, `--font-body`, `--font-mono`) and profiled in `brand.json`.

| Role | Family | Weights | Notes |
|---|---|---|---|
| Body / UI | **Inter Variable** | 300–700 | nav, links, buttons, body |
| Display / Heading | **Plus Jakarta Sans** | 200–800 | headings, hero, footer headings |
| Mono | JetBrains Mono (fallback chain) | 400 | derived — use for metrics/IDs only |

- Self-hosted woff2 in `fonts/`: `inter-latin-wght-normal.woff2`, `plus-jakarta-sans-latin-wght-normal.woff2`. Both families are **SIL OFL licensed** — free to use and redistribute.
- Base size **16px**, line-height **1.5**.
- Display face ≠ body face (Plus Jakarta Sans for headings, Inter for everything else) — satisfies the display/body distinction.
- Footer tagline is `font-weight: 300` (light), footer heading `600` semibold, mobile logo `700`.

### Type scale (derived, ratio ~1.25)
| Token | Size | Use |
|---|---|---|
| `--fs-xs` | 12px | captions, timestamps |
| `--fs-sm` | 14px | nav links, dropdown buttons, widget headers |
| `--fs-base` | 16px | body text |
| `--fs-lg` | 18px | footer links |
| `--fs-xl` | 20px | small headings |
| `--fs-2xl` | 24px | footer heading |
| `--fs-3xl` | 30px | section headings |
| `--fs-display` | clamp(32–56px) | hero headline |

---

## 4. Spacing

**Spacing scale is bound in `colors_and_type.css`** (`--space-*` tokens) and profiled in `brand.json`.

- Base unit: **4px** (`--space-1: 0.25rem`).
- Scale (rem / px): `0.25`/4 · `0.5`/8 · `0.75`/12 · `1`/16 · `1.25`/20 · `1.5`/24 · `2`/32 · `3`/48 · `4`/64 · `5`/80 · `8`/128.
- Radius: `0.375rem`/6 · `0.5rem`/8 (dropdowns, mobile links) · `0.75rem`/12 (cards) · `9999px` (pills, logo dot, round buttons).
- Density: comfortable for marketing; dashboard kit is denser (16px gap).
- Container max widths: **1180px** (default `max-w-[1180px]`) and **1280px** (`max-w-[1280px]`).

---

## 5. Layout & Composition

**Working reference:** `ui_kits/app/index.html` (applied marketing page) and `ui_kits/dashboard/index.html` (applied dashboard shell).

- **Sticky glass nav:** full-width, transparent→blur on scroll, `backdrop-filter: blur(2.5px)`, inset white highlight. Left: logo. Center/right (desktop): dropdown groups (Features, Solutions, Enterprise, Resources, Pricing), then ghost "Log in" + primary "Get started free" pill. Mobile: hamburger → full drawer with collapsible accordion sections.
- **Hero:** dark background, left-aligned headline + subcopy + primary CTA, right: dashboard illustration (`assets/imagery/hero.svg`). Eyebrow pill above headline.
- **Monitoring-type cards:** responsive grid (1 col mobile → 3 cols desktop) of `.monitoring-card` — glass card (`card-border-12`: radius 12, 1px `--neutral-700` border, `#131b25cc` bg, blur 10px), green icon in circle with drop-shadow glow, title, description, round "→" button. Hover: border → green, `translateY(-2px)`, green shadow `0 8px 25px #3bd67126`, icon glow intensifies.
- **Testimonials / enterprise demo:** glass panels with inset white highlight + `blur(2.5px)`.
- **Customer logos:** not part of this package — use your own grayscale logo set (replace the `preview/brand-assets.html` placeholder guidance).
- **Footer:** green-tinted gradient, large column layout, `Plus Jakarta Sans` 24px/600 headings with green `.` dot, `--neutral-200` light links `font-weight: 300`, social links scale 1.1 on hover.

**Responsive:** breakpoints `sm 640px`, `md 768px`, `lg 1024px`, `xl 1280px`. No horizontal scroll on mobile.

---

## 6. Components

Measured component shapes; working markup lives in `ui_kits/` and `preview/`:

### Buttons
- **Primary pill** (`.ur-nav-btn-primary`): `height 36px`, `padding 0 24px`, `border-radius 2rem`, `linear-gradient(135deg, primary-500 → primary-400)`, text `--neutral-900` (dark text on green), border `--neutral-600`, inset bottom shadow + outer white glow. Hover: overlay white gradient + green glow shadow `0 1px 12.6px #3bd67133`.
- **Ghost** (`.ur-nav-btn-ghost`): transparent, green text, `--neutral-600` border; hover adds the green glow.
- **Round button** (`.btn-round`): small circular, border → green on card hover.
- Focus-visible: `2px solid primary-500`, offset 2.
- **Dashboard additions:** secondary (surface outline), danger (red, destructive), icon-only, button-group. See §10.

### Navigation / dropdown
- Sticky glass bar. Dropdown button (`.ur-nav-dd-btn`): `--neutral-300`, 14px/500, `→` green on hover/active. Panel: `--neutral-950` bg, `2px solid --neutral-800`, radius `0.5rem`, fade+visibility transition, positioned `top: calc(100% + 28px)`. Invisible 30px bridge prevents hover gap.
- **Dashboard additions:** collapsible sidebar (240px → 64px icon-only), topbar search with ⌘K, tabs, breadcrumb. See §10.

### Cards
- **Monitoring card** (`.monitoring-card`): link card, glass surface, 12px radius, 1px `--neutral-700` border. Icon circle with `drop-shadow(0 0 23.9px rgba(59,214,113,.3))` glow, intensifies on hover. Hover lift `-2px` + green shadow. Transition `0.4s cubic-bezier(.25,.46,.45,.94)`.
- **Security feature card**: circle with gradient border via mask (`linear-gradient(90deg, #a3a7b100, #a3a7b1cc, #a3a7b100)`), green drop-shadow on parent hover.
- **Dashboard widget tile:** same glass surface but denser (16px gap, compact header), with optional drag-handle on hover and `⋯` menu. See §10.

### Dividers
- Horizontal/vertical security dividers use fade-in/out gradients (`transparent → --neutral-700 → transparent`).

### Footer
- Gradient bg, 4–5 link columns, `Plus Jakarta Sans` 24px/600 headings with green `.` dot, `--neutral-200` light links `font-weight: 300`, social links scale 1.1 on hover.

### Icons
- Inline `<symbol>` SVGs (`icon-checkmark`, `icon-chevron-right`, `icon-website-monitor`, `icon-keyword-monitoring`, etc.) filled with `#3BD671` or `currentColor`. No emoji icons.

---

## 7. Motion & Interaction

**Motion values are bound in `colors_and_type.css`** (`--transition-*` tokens).

- **Scroll reveal:** elements fade/slide in on `IntersectionObserver` (ScrollReveal). Staggered.
- **Card hover:** border-color → green, `translateY(-2px)`, green shadow, icon glow — all on `0.4s cubic-bezier(.25,.46,.45,.94)`.
- **Button hover:** white gradient overlay + green glow shadow, `transition: all .3s ease-out`.
- **Color/link transitions:** `0.15s cubic-bezier(.4,0,.2,1)`.
- **Dropdown:** opacity+visibility `0.2s`.
- **Logo ping:** `@keyframes ur-nav-ping` 2s infinite scale/opacity (status dot pulse).
- **Reduced motion:** respect `prefers-reduced-motion` — neutralize durations (bound in `colors_and_type.css`).

---

## 8. Voice & Brand

**Reference copy:** the demo copy in `ui_kits/app/index.html` mirrors this register.

### Voice posture
- **Direct and benefit-first** — every message answers "what does this do for me?"
- **Confident but plain** — no hype, no exclamation overload.
- **Operational and calm** — the product is reliable; the voice reflects that.
- **Technical enough for DevOps/SRE audiences**, but never gate-kept by jargon.

### Patterns
- Short imperative CTAs: "Start monitoring for free", "Get started free", "Monitor any HTTP(s) endpoint or page."
- Feature sentences lead with the verb: "Get alerted if a keyword appears or disappears", "Know if a scheduled job fails or goes missing".
- Concrete numbers only: "30 seconds", "50 monitors", "99.98%" — never round up invented stats.
- Sentence case for headings and nav; CTAs title-ish but informal.
- Vocabulary: uptime, monitors, endpoint, keyword, cron job, response time, SSL, alerts, integrations.

### Do not
- Marketing fluff or urgency-scarcity language.
- Invent metrics; use honestly-labelled placeholders when a real value is missing.
- Emoji or icon-as-copy replacements.

**Copy language:** match the target project's locale. The dashboard kit demos use German only for review context and should be translated to the target project locale before reuse.

---

## 9. Anti-patterns

Do NOT, when generating with this system:

- Introduce a second chromatic accent or a purple/blue/indigo gradient — green is the only accent; the primary button's gradient is green-to-lighter-green only.
- Use emoji as feature icons — use the inline SVG symbol set (or matched line/duotone SVGs filled with `currentColor`/`#3BD671`).
- Apply a `rounded card with left color-border accent` — cards are glass with a full 1px `--neutral-700` border that turns green on hover; no left-stripe accent.
- Hot-link remote images — copy assets into the project and reference by relative path only.
- Use Inter or a sans as the *display* face for hero/footer headings — headings use Plus Jakarta Sans; Inter is body/UI only.
- Add tracking scripts or third-party analytics — this system ships none.
- Ship a light/cream default canvas — this is a dark theme; `color-scheme: dark`.
- Fabricate metrics — the source uses real concrete numbers; placeholders must be honestly labelled, never invented.
- Place a badge/chip straddling a photo edge or over a focal subject — overlays pin to one corner, fully inside image bounds.
- Use bare-outline charts — encode with fills, not outlines.

---

## 10. Admin-Dashboard Components (extended kit)

A second applied kit in `ui_kits/dashboard/` extends this system into dark admin / monitoring dashboards — tables, forms, controls, stats boards, charts, navigation and overlays. It is the dashboard companion to the marketing kit (`ui_kits/app/`); the visual contract is identical. Status/chart tokens below are **derived** (oklch) from the measured green ramp — they are not a second brand accent.

### Dashboard tokens (in `colors_and_type.css`)
| Token | Value | Role |
|---|---|---|
| `--status-up` | `var(--primary-500)` | operational / healthy (= brand green) |
| `--status-warn` | `oklch(78% 0.15 75)` | degraded / partial / soon-expiring (derived) |
| `--status-down` | `oklch(60% 0.20 25)` | down / critical / failed (derived) |
| `--status-paused` | `oklch(62% 0.015 250)` | paused / inactive (derived) |
| `--status-info` | `oklch(72% 0.09 230)` | info / neutral hint (derived) |
| `--chart-line` | `var(--primary-500)` | primary series / trend |
| `--chart-area` | `oklch(78% 0.20 144 / 18%)` | chart fill |
| `--chart-grid` | `var(--neutral-700)` | grid lines |
| `--chart-2` / `--chart-3` | `--neutral-400` / `--neutral-500` | comparison series (neutral, not chromatic) |
| `--metric-pos` / `--metric-neg` | green / `--status-down` | positive / negative delta |
| `--row-hover` | `oklch(14% 0.013 250 / 60%)` | table row hover |
| `--widget-head` | `var(--neutral-400)` | widget header text |

### Layout
- **App shell:** `grid-template-columns:auto 1fr` — sidebar (240px, collapsible to 64px icon-only) + content (topbar 56px + breadcrumb + workspace head + filter bar + 12-column widget canvas). Gap 16px (denser than marketing's 20px). Min widget width 280px.
- **Responsive:** <1100px widgets reflow (1→half width); <768px sidebar becomes an off-canvas drawer, canvas collapses to one column, table gains horizontal scroll, forms stack.

### Component catalogue
- **Tables:** glass data grid, sticky sort headers with arrow, row hover (`--row-hover`), status badge per row, right-aligned mono numbers, hover-revealed row actions, bulk-selection bar, pagination, honest empty state.
- **Forms:** label-above fields, glass inputs with green focus ring (`box-shadow` halo), validation states (`is-error` red ring + message, `is-valid` subtle green), input affix (icon/prefix/suffix), fieldset grouping. Touch targets ≥40px.
- **Controls:** square checkbox with inline-SVG checkmark (no emoji), indeterminate bar, toggle switch, radio, segmented radio pills. `.check`/`.toggle`/`.radio` use a hidden input + styled sibling for accessible state.
- **Buttons:** extend the marketing pill — primary (green gradient), secondary (surface outline), ghost (green outline), danger (red, destructive), small, icon-only, button group.
- **Stats boards (fit the system):** KPI tile (large number + delta badge `↑/↓` green/red + mini sparkline), sparkline/area (fill, not outline), ring/donut (status distribution), horizontal bars (top errors), heatmap (check density). One green main series; comparison series in neutral tones — never rainbow.
- **Status:** pill badges with colored dot (`● Operational / Degraded / Down / Paused / Info`), pulsing status dot (reuses `ur-ping`), health-score ring, status grid of mini tiles.
- **Navigation:** collapsible sidebar with green active indicator (the documented exception to "no left-stripe" — it signals navigation, not a card), topbar with search + ⌘K, tabs, breadcrumb.
- **Overlays:** modal (glass backdrop), drawer (right detail panel), toast (status-coloured, auto-dismiss), tooltip.
- **Widget:** glass tile with header (title + drag-handle on hover + `⋯` menu) and content slot; sizes `w-1/w-2/w-3/w-full`; honest empty state for missing data.

### Dashboard anti-patterns (additions)
- Do NOT make dashboard charts rainbow or multi-accent — one green main series + neutral comparison tones only.
- Do NOT use outline-only charts — fill areas (`--chart-area`) over a line.
- Do NOT invent real metrics — demo data is honestly labelled; missing data renders an empty state, never a fabricated stat.
- Do NOT apply a left color-stripe to widgets — the **only** permitted left accent is the sidebar's active-link indicator, because it is navigation, not a card.

---

## 11. Implementation Notes

### File load order
1. `colors_and_type.css` — tokens + `@font-face` for self-hosted fonts.
2. `ui_kits/app/kit.css` or `ui_kits/dashboard/kit.css` — component layer (each `@import`s the tokens).
3. Page-specific overrides only when necessary — and only by re-mapping existing tokens, never by inventing raw hex.

### Reuse workflow
- Read `DESIGN.md` and `brand.json` for rules and token roles.
- Paste or `@import` `colors_and_type.css` verbatim as the first style layer.
- Load self-hosted fonts from `fonts/` (or rely on the `@font-face` in the token file).
- Copy component markup from `ui_kits/app/` (marketing) or `ui_kits/dashboard/` (admin).
- Keep it dark, keep it green, keep it glass.

### Light mode
There is **no source-backed light theme**. If a project explicitly requires a light mode, it must be built as a separate token layer with its own `--bg`, `--surface`, `--fg`, `--muted`, `--border`, `--accent` values. Do not try to "auto-derive" a light palette from the dark tokens by inversion — that is not how this system was authored.

### Assets
- `assets/brand/wordmark.svg` and `assets/imagery/*.svg` are generic, token-derived placeholders — replace with real brand assets before shipping a real product.
- Reference assets by relative path only; never hot-link remote images.
