package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// RED tests for the SPA serving layer (plan §9, Phase 2 research E-1..E-4,
// §5 decision, P-1..P-5). Derived from the research contracts, never from
// implementation behavior:
//
//   - E-3: the embedded FS plugs into the stock file server; the spa
//     handler only decorates the fallback decision.
//   - §5:  GET, no matching file, not under /v1/ or /api/, Accept
//     includes text/html → index.html with 200 (the SPA renders its own
//     404 view). Everything else → 404.
//   - P-1..P-5: every WebUI response carries the strict CSP + security
//     headers. API responses (non-SPA paths) carry the same base headers.
//
// The handler takes an fs.FS so tests run against a synthetic dist tree —
// no dependency on a Node build.

func newTestSPA(t *testing.T) http.Handler {
	t.Helper()
	dist := fstest.MapFS{
		"index.html":        &fstest.MapFile{Data: []byte("<!doctype html><html><body>SPA index</body></html>")},
		"assets/app.js":     &fstest.MapFile{Data: []byte("// bundle")},
		"assets/app.css":    &fstest.MapFile{Data: []byte("body{}")},
		"fonts/inter.woff2": &fstest.MapFile{Data: []byte("font-bytes")},
	}
	return NewSPAHandler(dist)
}

// E-3 / §5: a real asset path serves the file.
func TestSPA_ServesAsset(t *testing.T) {
	h := newTestSPA(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/assets/app.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("asset request: got status %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != "// bundle" {
		t.Fatalf("asset body: got %q, want %q", body, "// bundle")
	}
}

// E-3 / §5: index.html serves directly.
func TestSPA_ServesIndex(t *testing.T) {
	h := newTestSPA(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("root request: got status %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "SPA index") {
		t.Fatalf("index body: got %q, want the SPA index", body)
	}
}

// §5: a deep link that matches no file falls back to index.html — 200, not
// 404 — so the SPA renders the route (and its own 404 view for unknown
// paths) client-side.
func TestSPA_DeepLinkFallsBackToIndex(t *testing.T) {
	h := newTestSPA(t)
	for _, path := range []string{"/models", "/logs", "/providers/refresh"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Accept", "text/html,application/xhtml+xml")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Errorf("deep link %s: got status %d, want 200 (index fallback)", path, rec.Code)
		}
		if body := rec.Body.String(); !strings.Contains(body, "SPA index") {
			t.Errorf("deep link %s: body does not contain the SPA index", path)
		}
	}
}

// §5: non-GET, non-HTML, /v1//api paths never fall back — they 404.
func TestSPA_NoFallbackForAPIOrNonGET(t *testing.T) {
	h := newTestSPA(t)
	cases := []struct {
		method string
		path   string
		accept string
	}{
		{"GET", "/api/admin/providers", "text/html"},
		{"GET", "/v1/models", "text/html"},
		{"POST", "/models", "text/html"},
		{"GET", "/assets/missing.js", "text/html"},
		{"GET", "/models", "application/json"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		r.Header.Set("Accept", c.accept)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s (Accept %s): got status %d, want 404", c.method, c.path, c.accept, rec.Code)
		}
	}
}

// P-1..P-5: the CSP + security headers ride on SPA responses. The exact
// policy is pinned by the research (research §4 P-3): default-src 'self',
// script-src 'self', style-src 'self', img-src 'self' data:, font-src
// 'self', connect-src 'self', object-src 'none', base-uri 'self',
// form-action 'self', frame-ancestors 'none'.
func TestSPA_SecurityHeaders(t *testing.T) {
	h := newTestSPA(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	wantCSP := "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
		"font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'self'; " +
		"form-action 'self'; frame-ancestors 'none'"
	if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP {
		t.Fatalf("CSP header: got %q, want %q", got, wantCSP)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options: got %q, want nosniff", got)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "same-origin" {
		t.Errorf("Referrer-Policy: got %q, want same-origin", got)
	}
}
