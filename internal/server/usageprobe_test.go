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
	"time"

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
	if len(usage.Windows) != 2 {
		t.Fatalf("windows = %+v, want the derived session+weekly entries", usage.Windows)
	}
	if usage.Windows[0].Key != "session" || usage.Windows[0].Source != "ollama_settings" ||
		usage.Windows[0].RemainingPct == nil || *usage.Windows[0].RemainingPct < 71.1 ||
		*usage.Windows[0].RemainingPct > 71.3 {
		t.Errorf("session window = %+v, want remaining 71.2 with the settings source", usage.Windows[0])
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

// The official balance API is the preferred Ollama source: a Bearer key
// (no browser cookie), remaining percentages and reset instants.
func TestFetchOllamaBalance_LegacyWindows(t *testing.T) {
	var gotPath, gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{
			"included": {
				"session": {"remaining_percent": 68.23, "resets_at": "2026-10-25T16:00:00Z"},
				"weekly":  {"remaining_percent": 78.37, "resets_at": "2026-10-26T00:00:00Z"}
			},
			"purchased": {"balance_usd": 0}
		}`))
	}))
	defer ts.Close()

	blob, err := fetchOllamaBalance(context.Background(), ts.URL, "ollama-key", ts.Client())
	if err != nil {
		t.Fatalf("fetchOllamaBalance: %v", err)
	}
	if gotPath != "/api/balance" {
		t.Errorf("path = %q, want /api/balance", gotPath)
	}
	if gotAuth != "Bearer ollama-key" {
		t.Errorf("Authorization = %q, want the Bearer key", gotAuth)
	}
	if len(blob.Windows) != 2 {
		t.Fatalf("windows = %+v, want session+weekly", blob.Windows)
	}
	session := blob.Windows[0]
	if session.Key != "session" || session.Source != "ollama_balance" ||
		session.RemainingPct == nil || *session.RemainingPct != 68.23 ||
		session.UsedPct == nil || *session.UsedPct < 31.76 || *session.UsedPct > 31.78 {
		t.Errorf("session = %+v, want 68.23 remaining / 31.77 used", session)
	}
	if session.ResetAt != parseRFC3339Unix("2026-10-25T16:00:00Z") {
		t.Errorf("session reset = %d", session.ResetAt)
	}
	if blob.Windows[1].Key != "weekly" {
		t.Errorf("second window = %+v, want weekly", blob.Windows[1])
	}
}

// Monthly plans carry a USD balance and billing period; purchased credit is
// a separate window when it is non-zero.
func TestFetchOllamaBalance_MonthlyAndPurchased(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"included": {
				"balance_usd": 72.5,
				"allowance_usd": 100,
				"period": {"from": "2026-10-01T00:00:00Z", "until": "2026-11-01T00:00:00Z"}
			},
			"purchased": {"balance_usd": 5}
		}`))
	}))
	defer ts.Close()

	blob, err := fetchOllamaBalance(context.Background(), ts.URL, "ollama-key", ts.Client())
	if err != nil {
		t.Fatalf("fetchOllamaBalance: %v", err)
	}
	if len(blob.Windows) != 2 {
		t.Fatalf("windows = %+v, want monthly+purchased", blob.Windows)
	}
	monthly := blob.Windows[0]
	if monthly.Key != "monthly" || monthly.Kind != "usd" ||
		monthly.Remaining == nil || *monthly.Remaining != 72.5 ||
		monthly.Limit == nil || *monthly.Limit != 100 {
		t.Errorf("monthly = %+v, want $72.50 of $100", monthly)
	}
	if monthly.WindowSeconds != 31*24*60*60 {
		t.Errorf("window seconds = %d, want 31 days", monthly.WindowSeconds)
	}
	if blob.Windows[1].Key != "purchased" || blob.Windows[1].Remaining == nil ||
		*blob.Windows[1].Remaining != 5 {
		t.Errorf("purchased = %+v, want $5", blob.Windows[1])
	}
}

// A balance payload with no recognized window is an error, never invented
// zeros.
func TestFetchOllamaBalance_NoUsageIsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"included":{},"purchased":{"balance_usd":0}}`))
	}))
	defer ts.Close()
	if _, err := fetchOllamaBalance(context.Background(), ts.URL, "ollama-key", ts.Client()); err == nil {
		t.Fatal("a balance payload with no window must be an error")
	}
}

// The Firecrawl credit window (Quota page): the documented
// GET /v2/team/credit-usage shape maps onto the blob's credit fields, and
// the billing-period end becomes the card's reset timestamp.
func TestFetchFirecrawlUsage(t *testing.T) {
	var gotPath, gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{
			"success": true,
			"data": {
				"remainingCredits": 850,
				"planCredits": 1000,
				"billingPeriodStart": "2026-10-01T00:00:00Z",
				"billingPeriodEnd": "2026-11-01T00:00:00Z"
			}
		}`))
	}))
	defer ts.Close()

	blob, err := fetchFirecrawlUsage(context.Background(), ts.URL, "fc-key", ts.Client())
	if err != nil {
		t.Fatalf("fetchFirecrawlUsage: %v", err)
	}
	if gotPath != "/v2/team/credit-usage" {
		t.Errorf("path = %q, want /v2/team/credit-usage", gotPath)
	}
	if gotAuth != "Bearer fc-key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if blob.RemainingCredits == nil || *blob.RemainingCredits != 850 {
		t.Errorf("remaining = %v, want 850", blob.RemainingCredits)
	}
	if blob.PlanCredits == nil || *blob.PlanCredits != 1000 {
		t.Errorf("plan = %v, want 1000", blob.PlanCredits)
	}
	wantReset := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Unix()
	if blob.CreditsReset != wantReset {
		t.Errorf("reset = %d, want %d", blob.CreditsReset, wantReset)
	}
	if !blob.HasWindows() {
		t.Error("a credit window must count as a live window")
	}
	if len(blob.Windows) != 1 || blob.Windows[0].Key != "credits" ||
		blob.Windows[0].Source != "firecrawl_credit_usage" ||
		blob.Windows[0].Remaining == nil || *blob.Windows[0].Remaining != 850 {
		t.Errorf("windows = %+v, want one firecrawl credit window", blob.Windows)
	}
}

// A success:false body is an error, never a zero-credit reading.
func TestFetchFirecrawlUsage_SuccessFalseIsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"error":"Could not find credit usage information"}`))
	}))
	defer ts.Close()

	if _, err := fetchFirecrawlUsage(context.Background(), ts.URL, "fc-key", ts.Client()); err == nil {
		t.Fatal("success:false must be an error")
	}
}
