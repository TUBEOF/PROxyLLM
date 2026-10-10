package server

// Usage prober tests (ADR-012). The parser is a pure function over the
// ollama.com/settings HTML — fixture-pinned (testdata/golden/ollama/
// settings-usage.html, shape pinned from the served page,
// accessed 2026-10-06). The fetch layer is injectable; the
// production client hits a FIXED host (no SSRF surface) with the
// account's usage cookie, cookie and body never logged.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tubeof/proxyllm/internal/translate/testutil"
)

func TestParseOllamaSettingsUsage(t *testing.T) {
	html := testutil.LoadGolden(t, "ollama/settings-usage.html")
	usage, err := parseOllamaSettingsHTML(html)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if usage == nil {
		t.Fatal("usage = nil, want parsed windows")
	}
	if usage.SessionPct == nil || *usage.SessionPct < 28.7 || *usage.SessionPct > 28.9 {
		t.Fatalf("session percent = %v, want 28.8 (aria-label)", usage.SessionPct)
	}
	if usage.SessionReset != 1_759_000_000 {
		t.Fatalf("session reset = %d, want 1759000000 (data-time)", usage.SessionReset)
	}
	if usage.WeeklyPct == nil || *usage.WeeklyPct < 27.3 || *usage.WeeklyPct > 27.5 {
		t.Fatalf("weekly percent = %v, want 27.4 (width style)", usage.WeeklyPct)
	}
	if usage.WeeklyReset != 1_759_500_000 {
		t.Fatalf("weekly reset = %d, want 1759500000", usage.WeeklyReset)
	}
}

func TestParseOllamaSettingsUsage_EmptyPage(t *testing.T) {
	usage, err := parseOllamaSettingsHTML([]byte(`<html><body>signed out</body></html>`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if usage != nil {
		t.Fatalf("usage = %+v, want nil (no data-usage-track)", usage)
	}
}

// The fetch layer: fixed host, the session cookie rides the request,
// and a non-200 or a signed-out page is an error the caller degrades on.
func TestProbeOllamaUsage_SendsCookieAndRejectsSignedOut(t *testing.T) {
	var gotCookie, gotPath string
	var status int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		gotPath = r.URL.Path
		if status == 0 {
			_, _ = w.Write([]byte(`<html><body>signed out</body></html>`))
			return
		}
		_, _ = w.Write([]byte(`<div data-usage-track aria-label="5% used"></div>`))
	}))
	defer ts.Close()

	prober := newUsageProber(ts.URL, http.DefaultClient)

	// Signed-out page (no data-usage-track): an error, degraded — not a
	// crash.
	_, err := prober.Probe(context.Background(), "session-cookie-1")
	if err == nil {
		t.Fatal("signed-out page must be an error (no usage tracks)")
	}
	if gotCookie != "__Secure-session=session-cookie-1" {
		t.Fatalf("cookie header = %q", gotCookie)
	}
	if gotPath != "/settings" {
		t.Fatalf("path = %q, want /settings (fixed)", gotPath)
	}
}
