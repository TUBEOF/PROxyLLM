// Package webui serves the embedded single-page app and applies the WebUI
// security headers (Phase 2 research §4 P-1..P-5, §5 decision option 2).
package webui

import (
	"io/fs"
	"net/http"
	"strings"
)

// csp is the pinned Content-Security-Policy (research §4). The Vite build
// emits zero inline scripts (SPA mode, assetsInlineLimit: 0), so no nonces
// are required: script-src 'self' alone admits the bundles. data: stays in
// img-src only as headroom for inline SVG data URIs if ever needed; fonts
// are self-hosted files.
const csp = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// NewSPAHandler returns a handler serving the built SPA from fsys.
//
// The embedded FS plugs into the stock file server (research E-3); the only
// decoration is the history-routing fallback (§5 option 2): a GET request
// that accepts HTML, maps to no file, and is not under /v1/ or /api/ serves
// index.html with 200 so deep links survive a hard refresh — the SPA renders
// its own 404 view client-side for unknown paths. Everything else keeps the
// stock file-server behavior (404 for missing files).
func NewSPAHandler(fsys fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(fsys))
	index, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		// The committed placeholder dist/index.html guarantees this never
		// happens in a real build; a broken embed fails the build earlier.
		panic("webui: dist is missing index.html: " + err.Error())
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Security headers ride on every response this handler produces
		// (P-1..P-5).
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")

		if r.Method != http.MethodGet {
			fileServer.ServeHTTP(w, r)
			return
		}

		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" {
			if _, err := fs.Stat(fsys, path); err == nil {
				fileServer.ServeHTTP(w, r)
				return
			}
		} else {
			// "/" maps to index.html via the file server itself.
			fileServer.ServeHTTP(w, r)
			return
		}

		// No file matched. API surfaces, asset paths, and non-HTML
		// requests keep the stock 404 — only browser navigations fall
		// back to the SPA. Assets 404 deliberately: falling back to
		// index.html there would mask a broken build as silent HTML.
		if strings.HasPrefix(r.URL.Path, "/v1/") ||
			strings.HasPrefix(r.URL.Path, "/api/") ||
			strings.HasPrefix(r.URL.Path, "/assets/") ||
			!acceptsHTML(r) {
			fileServer.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(index)
	})
}

// acceptsHTML reports whether the client is a browser navigation. Browsers
// always list text/html in Accept; API clients (curl defaults, SDKs) do not.
func acceptsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}
