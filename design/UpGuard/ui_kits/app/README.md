# Applied UI Kit — UpGuard

An applied interface kit reflecting a dark, glassmorphic monitoring-product homepage. Every component is built on the design-system tokens (`../../colors_and_type.css`) via `kit.css`. This is a reusable Claude Design-style package — copy the files, re-point paths, and the kit stays on-system.

## Structure

| Path | What |
|---|---|
| `index.html` | Launcher + live demo (nav, hero, status strip, monitoring cards, footer). Start here. |
| `kit.css` | Component layer — `@import`s the tokens, declares `@font-face` for the self-hosted fonts, and defines every component class. Paste after the token layer; never override tokens here. |
| `components/buttons.html` | Primary pill, ghost, round CTA, dropdown trigger, mobile burger. |
| `components/nav.html` | Sticky glass nav with a working click-to-open dropdown + mobile burger. |
| `components/cards.html` | Glass monitoring cards, testimonial panel, divider. |
| `components/footer.html` | Green-tinted gradient footer with link columns. |

> Brand, logo, and imagery are placeholders. Replace the wordmark and any on-brand images before a public deploy.

## Source basis

Built entirely on this design system: tokens from `../../colors_and_type.css`, component rules from `../../DESIGN.md` and `../../brand.json`. The kit uses a neutral placeholder name (UpGuard); replace it for any public deploy.

## Components

(Component → token-bound class; see kit.css for the full list.)

- **Buttons**

- **Buttons** → `.btn` / `.btn-primary` / `.btn-ghost` / `.btn-round`
- **Navigation** → `.nav` / `.nav-inner` / `.nav-links` / `.nav-link` / `.dd` / `.dd-btn` / `.dd-panel` / `.burger`
- **Hero** → `.hero` / `.eyebrow` / `.hero-img` / `.hero-cta`
- **Status strip** → `.status` / `.stat` / `.stat .v.ok`
- **Cards** → `.grid` / `.grid-3` / `.card` / `.icon-circle`
- **Testimonial** → `.testimonial`
- **Divider** → `.divider`
- **Footer** → `.footer` / `.footer-inner` / `.footer h4 .dot`

## Usage workflow

1. Copy `kit.css` and the `../../fonts/` folder into your project, or re-point the `@import` and `@font-face` `url()` paths at the top of `kit.css`.
2. Link `colors_and_type.css` **then** `kit.css` in your HTML (tokens first; kit consumes them).
3. Use the class names from `kit.css` directly (`.btn`, `.btn-primary`, `.card`, `.icon-circle`, `.nav`, `.footer`, …) — they are bound to the tokens, so they stay on-system.
4. For a full page, copy the structure from `index.html`; for single components, open the matching `components/*.html`.

## Design notes

- **Porting to React:** each `components/*.html` maps 1:1 to a `components/*.jsx` file — e.g. `components/Nav.jsx` (nav + dropdown), `components/Buttons.jsx`, `components/Cards.jsx`, `components/Footer.jsx` — with the App shell composing them.

- **Dark canvas + one green accent.** `--bg #111821`, `--accent #3bd671`. Never add a second chromatic accent.
- **Glass cards** = translucent surface + `blur(10px)` + 1px border → green on hover with `-2px` lift and green shadow. No left-stripe accent.
- **Primary button** = green-gradient pill, dark text, green-glow hover.
- **Display ≠ body.** Plus Jakarta Sans for headings; Inter for body/UI.
- **Icons** = inline SVG filled `currentColor`/`#3BD671`; no emoji.
- **Motion** is restrained and honors `prefers-reduced-motion` (status-dot ping disabled).
- **Imagery** is referenced by relative path from `../../assets/imagery/` (token-derived SVGs). Never hot-link remote URLs.
