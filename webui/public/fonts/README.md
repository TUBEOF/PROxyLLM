# Fonts — the one font source

The console's fonts live here and nowhere else; `design/fonts/README.md` points
at this directory instead of duplicating the files. Vite copies `public/` into
the built SPA, so everything here is served from `/fonts/`, which is the URL the
`@font-face` rules in `colors_and_type.css` use.

| File | Family | Weights | Declared in the token layer | Licence |
|---|---|---|---|---|
| `inter-latin-wght-normal.woff2` | Inter (variable) | 100–1000 | yes | SIL OFL 1.1 |
| `plus-jakarta-sans-latin-wght-normal.woff2` | Plus Jakarta Sans (variable) | 200–800 | yes | SIL OFL 1.1 |
| `jetbrains-mono-regular.woff2` | JetBrains Mono | 400 | yes | SIL OFL 1.1 |
| `jetbrains-mono-medium.woff2` | JetBrains Mono | 500 | yes | SIL OFL 1.1 |

All three families are licensed under the SIL Open Font License 1.1: they may be
used, embedded and redistributed, including in commercial and open-source
projects, and this repository ships them for exactly that purpose. Keep a licence
notice with the files when redistributing; none of the three declares a Reserved
Font Name.

JetBrains Mono comes from the upstream release `v2.304`; its full licence text is
`OFL-JetBrainsMono.txt` and its copyright holders are listed in
`AUTHORS-JetBrainsMono.txt` (both shipped here). The two files are the Regular and
Medium weights of that release's static webfonts.

**Declared.** The two faces are declared in `colors_and_type.css`, so the mono cells —
numbers, client keys, timestamps, pills — render in JetBrains Mono instead of resolving
through the fallback stack. The glyphs of those cells therefore differ from the committed
Playwright screenshot baselines, which have to be regenerated through the
`update-visual-baselines` workflow.
