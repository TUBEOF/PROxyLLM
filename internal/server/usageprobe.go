package server

// Usage prober (ADR-012): fetches one account's usage/limit state from
// ollama.com. The target is a FIXED host — operator or requester input
// can never point it elsewhere (no SSRF surface; the SSRF guard rule,
// docs/security.md). The account's session cookie authenticates the scrape;
// the cookie and the page body are never logged (redactor + no log call
// here at all).
//
// Parser shape pinned from the served page (accessed
// 2026-10-06): the settings page carries `data-usage-track` blocks —
// percent from the aria-label ("X% used" / "$X of $Y used") or the
// width style; reset timestamps from a `.local-time` element's
// data-time. First track = session, second = weekly.
//
// The refresh pipeline (account validation → cookie read → scrape →
// persist) lives HERE so webui only injects the func — the admin params
// carry a plain closure, production is wired in server.New.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tubeof/proxyllm/internal/httpguard"
	"github.com/tubeof/proxyllm/internal/server/webui"
	"github.com/tubeof/proxyllm/internal/storage"
)

// ollamaSettingsUsageURL is the fixed scrape target origin. The path is
// appended per request — never operator-supplied (SSRF guard).
const ollamaSettingsUsageURL = "https://ollama.com"

// ollamaSettingsUsagePath is the fixed scraping path on the origin.
const ollamaSettingsUsagePath = "/settings"

// probeTimeout bounds one scrape.
const probeTimeout = 15 * time.Second

// maxUsageHTMLBytes bounds the body read (the settings page is small).
const maxUsageHTMLBytes = 1 << 22 // 4 MiB

// usageProber fetches and parses one account's usage. baseURL is
// injectable (tests point it at an httptest server); production pins
// the constant. The client is the guarded one (httpguard).
type usageProber struct {
	baseURL string
	http    *http.Client
}

// newUsageProber builds the prober; the test server URL (origin only,
// no trailing path) is appended with the fixed /settings path.
func newUsageProber(baseURL string, client *http.Client) *usageProber {
	if baseURL == "" {
		baseURL = ollamaSettingsUsageURL
	}
	return &usageProber{baseURL: baseURL, http: client}
}

// Probe scrapes the settings page with the given RAW session cookie
// value and returns the parsed windows. A signed-out page (no
// data-usage-track) is an error — the caller stores it into the blob's
// Error field and the card renders that error.
func (p *usageProber) Probe(ctx context.Context, sessionCookie string) (*storage.AccountUsageBlob, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+ollamaSettingsUsagePath, nil)
	if err != nil {
		return nil, fmt.Errorf("usage prober: build request: %w", err)
	}
	req.Header.Set("Cookie", "__Secure-session="+sessionCookie)
	// A browser-shaped UA: the settings page serves signed-in HTML only
	// to browsers (the page serves signed-in HTML only to browser-shaped clients).
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) proxyllm-usage-probe")

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("usage prober: fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUsageHTMLBytes))
	if err != nil {
		return nil, fmt.Errorf("usage prober: read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("usage prober: ollama.com answered HTTP %d", resp.StatusCode)
	}
	usage, err := parseOllamaSettingsHTML(body)
	if err != nil {
		return nil, err
	}
	if usage == nil {
		return nil, errors.New("usage prober: settings page carries no usage data (signed out?)")
	}
	usage.FetchedAt = time.Now().Unix()
	return usage, nil
}

// percentFromAria extracts X from "X% used" (the page's own phrasing).
var percentFromAria = regexp.MustCompile(`(\d+(?:\.\d+)?)%\s*used`)

// dollarsFromAria extracts "$X of $Y used" (pro accounts with dollar
// quotas); the percent is X/Y.
var dollarsFromAria = regexp.MustCompile(`\$\s*([0-9.]+)\s*of\s*\$\s*([0-9.]+)\s*used`)

// widthStyle matches `width: X%` behind a quote or semicolon (inline
// style attribute value — the HTML shape of the track's fill bar).
var widthStyle = regexp.MustCompile(`[";]\s*width\s*:\s*([0-9.]+)%`)

// dataTimeAttr extracts the unix timestamp of a `.local-time` element.
var dataTimeAttr = regexp.MustCompile(`class="[^"]*local-time[^"]*"[^>]*data-time="([^"]*)"`)

// parseOllamaSettingsHTML extracts the usage windows from the settings
// HTML. nil (no error) when the page carries no usage tracks (signed
// out) — the caller degrades. Never logs the body or cookie.
func parseOllamaSettingsHTML(html []byte) (*storage.AccountUsageBlob, error) {
	parts := strings.Split(string(html), "data-usage-track")
	if len(parts) < 2 {
		return nil, nil
	}
	usage := &storage.AccountUsageBlob{}
	usage.SessionPct = extractUsagePercent(parts[1])
	usage.SessionReset = firstTimeAttr(parts[1])
	if len(parts) > 2 {
		usage.WeeklyPct = extractUsagePercent(parts[2])
		usage.WeeklyReset = firstTimeAttr(parts[2])
	}
	return usage, nil
}

// extractUsagePercent pulls the percent out of one usage-track block:
// aria-label first ("X% used", "$X of $Y used"), then the width style.
func extractUsagePercent(block string) *float64 {
	tagHeader := block
	if i := strings.IndexByte(block, '>'); i >= 0 {
		tagHeader = block[:i]
	}
	if m := percentFromAria.FindStringSubmatch(tagHeader); m != nil {
		return clampPctPtr(parsePct(m[1]))
	}
	if m := dollarsFromAria.FindStringSubmatch(tagHeader); m != nil {
		used, err1 := strconv.ParseFloat(m[1], 64)
		total, err2 := strconv.ParseFloat(m[2], 64)
		if err1 == nil && err2 == nil && total > 0 {
			return clampPctPtr(used / total * 100)
		}
	}
	// The width style can sit in ANY tag of the block.
	for _, m := range widthStyle.FindAllStringSubmatch(block, -1) {
		if p := clampPctPtr(parsePct(m[1])); p != nil {
			return p
		}
	}
	return nil
}

// firstTimeAttr extracts the first data-time of a `.local-time` element
// in the block (unix seconds as decimal string).
func firstTimeAttr(block string) int64 {
	if m := dataTimeAttr.FindStringSubmatch(block); m != nil {
		if t, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			return t
		}
	}
	return 0
}

func parsePct(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return -1
	}
	return v
}

func clampPctPtr(v float64) *float64 {
	if v < 0 || v > 100 || v != v {
		return nil
	}
	return &v
}

// probeStatuses maps refresh failures to the admin refresh endpoint's
// response shape.
const (
	statusMissingCookie   = http.StatusBadRequest
	statusUnknownAccount  = http.StatusNotFound
	statusProbeFailed     = http.StatusBadGateway
	statusUnsupportedType = http.StatusBadRequest
)

// prodUsageProber is the production probe closure: account validation →
// cookie read (encrypted token store kind usage_cookie) → fixed-host
// scrape → KV persist. Injected into webui.AdminParams.Prober by New —
// the admin endpoint stays thin and stubbable.
func (s *Server) prodUsageProber() func(ctx context.Context, accountID string, store *storage.Store) webui.RefreshOutcome {
	prober := newUsageProber("", httpguard.NewClient(probeTimeout))
	return func(ctx context.Context, accountID string, store *storage.Store) webui.RefreshOutcome {
		acct, err := store.GetProvider(ctx, accountID)
		if err != nil {
			return webui.RefreshOutcome{Status: statusUnknownAccount, Text: "unknown account"}
		}
		if acct.Type != "ollama-cloud" && acct.Type != "ollama-cloud-web" {
			return webui.RefreshOutcome{Status: statusUnsupportedType,
				Text: "usage scraping is implemented for the ollama.com account types (ollama-cloud, ollama-cloud-web)"}
		}
		tok, err := store.GetProviderToken(ctx, accountID, webui.UsageCookieTokenKind)
		if errors.Is(err, storage.ErrNotFound) {
			return webui.RefreshOutcome{Status: statusMissingCookie,
				Text: "account has no usage cookie — set it to enable live usage"}
		}
		if err != nil {
			return webui.RefreshOutcome{Status: http.StatusInternalServerError, Text: "token store failure"}
		}
		blob, err := prober.Probe(ctx, string(tok.Secret))
		if err != nil {
			// Honest error state in the blob (the card shows it); the
			// message carries no cookie/page material by construction.
			_ = store.PutAccountUsage(ctx, accountID, storage.AccountUsageBlob{
				Error: truncateProbeError(err.Error()),
			})
			return webui.RefreshOutcome{Status: statusProbeFailed, Text: "usage scrape failed: " + truncateProbeError(err.Error())}
		}
		if err := store.PutAccountUsage(ctx, accountID, *blob); err != nil {
			return webui.RefreshOutcome{Status: http.StatusInternalServerError, Text: "usage blob persist failed"}
		}
		return webui.RefreshOutcome{OK: true}
	}
}

// truncateProbeError bounds error text (defense in depth; the prober
// builds its own messages and never embeds cookie or body material).
func truncateProbeError(msg string) string {
	const cap = 200
	if len(msg) > cap {
		return msg[:cap]
	}
	return msg
}
