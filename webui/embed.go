// Package webui embeds the built single-page app (Vite output in dist/)
// into the Go binary. The embed directive uses `all:` so files whose names
// begin with `.` or `_` are included too (Phase 2 research E-1: without
// the prefix, such assets silently 404 after embedding).
//
// dist/ is a build artifact (gitignored) except for the committed
// placeholder index.html: a fresh clone compiles without a Node toolchain,
// and CI runs `npm ci && npm run build` before `go build` so the real
// assets replace the placeholder. There is intentionally no embed of any
// source file from this directory — only the built output.
package webui

import "embed"

// Dist holds the built SPA. The placeholder index.html committed to
// dist/ guarantees the pattern always matches (go:embed errors on a
// pattern that matches no file).
//
//go:embed all:dist
var Dist embed.FS
