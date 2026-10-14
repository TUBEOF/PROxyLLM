# Fonts

This directory holds the pointer, not the files. The console's fonts have a
single source: `../../webui/public/fonts/`, which ships with the SPA and is
served from `/fonts/` — the URL the `@font-face` rules in `colors_and_type.css`
use. Keeping a second copy here would mean two sets of font binaries that can
drift apart.

| Family | Files | Licence |
|---|---|---|
| Inter (variable, 100–1000) | `inter-latin-wght-normal.woff2` | SIL OFL 1.1 |
| Plus Jakarta Sans (variable, 200–800) | `plus-jakarta-sans-latin-wght-normal.woff2` | SIL OFL 1.1 |
| JetBrains Mono (400, 500) | `jetbrains-mono-regular.woff2`, `jetbrains-mono-medium.woff2` | SIL OFL 1.1 — full text in `OFL-JetBrainsMono.txt`, copyright holders in `AUTHORS-JetBrainsMono.txt` |

All three are free to use, embed and redistribute, commercially included; keep
the licence notice with the files when you pass them on. All three are declared
by the token layer and render in the console — read
`../../webui/public/fonts/README.md` before you change which face the console
renders, because that is a visible step.
