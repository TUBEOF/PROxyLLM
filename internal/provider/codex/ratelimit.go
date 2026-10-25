package codex

// The ChatGPT backend reports an account's rate limits on the responses of
// live /codex/responses traffic — there is no usage endpoint to probe. The
// header family is codex-rs's own (codex-api/src/rate_limits.rs, accessed
// 2026-10-25):
//
//	x-codex-primary-used-percent      f64, percent of the window used
//	x-codex-primary-window-minutes    i64
//	x-codex-primary-reset-at          i64, unix seconds
//	x-codex-secondary-used-percent    f64 (the weekly window)
//	x-codex-secondary-window-minutes  i64
//	x-codex-secondary-reset-at        i64, unix seconds
//
// Additional limit families use x-<limit-id>-<primary|secondary>-… and
// x-<limit-id>-limit-name; credits use
// x-codex-credits-{has-credits,unlimited,balance}. A `codex.rate_limits`
// SSE event carries the same facts on the stream, which the tap below reads.
//
// A snapshot exists only when the backend sent at least one used-percent or
// a credit fact — zeros are never invented.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// RateLimitWindow is one metered window: used percent, length, reset.
type RateLimitWindow struct {
	UsedPct       *float64
	WindowMinutes int64
	ResetAt       int64
}

// RateLimitCredits is the credit balance family, when reported. Balance is
// nil when the backend sent a non-numeric string (Raw keeps the original).
type RateLimitCredits struct {
	Present    bool
	HasCredits bool
	Unlimited  bool
	Balance    *float64
	Raw        string
}

// RateLimitFamily is one named limit family (the default legacy family is
// "codex"; extra families use the backend's limit id).
type RateLimitFamily struct {
	ID        string
	Name      string
	Primary   *RateLimitWindow
	Secondary *RateLimitWindow
}

// RateLimits is one account's rate-limit state, read off a live response.
// The legacy mirrors stay for the existing persistence contract; Families
// and Credits carry the full snapshot.
type RateLimits struct {
	PrimaryUsedPct         *float64
	PrimaryResetAt         int64
	SecondaryUsedPct       *float64
	SecondaryResetAt       int64
	PrimaryWindowMinutes   int64
	SecondaryWindowMinutes int64
	Families               []RateLimitFamily
	Credits                *RateLimitCredits
	Source                 string // codex_headers | codex_sse
}

const maxRateLimitFamilies = 8

// rateLimitFromHeaders parses the x-codex-* family. ok is false when no
// used-percent or credit fact arrived — a response the backend did not meter
// (or a third party's error page) must not persist a fake snapshot.
func rateLimitFromHeaders(h http.Header) (RateLimits, bool) {
	rl := RateLimits{Source: "codex_headers"}
	primary := windowFromHeaders(h, "x-codex-primary")
	secondary := windowFromHeaders(h, "x-codex-secondary")
	if primary != nil {
		rl.PrimaryUsedPct = primary.UsedPct
		rl.PrimaryResetAt = primary.ResetAt
		rl.PrimaryWindowMinutes = primary.WindowMinutes
	}
	if secondary != nil {
		rl.SecondaryUsedPct = secondary.UsedPct
		rl.SecondaryResetAt = secondary.ResetAt
		rl.SecondaryWindowMinutes = secondary.WindowMinutes
	}
	if primary != nil || secondary != nil {
		rl.Families = append(rl.Families, RateLimitFamily{
			ID: "codex", Name: "Codex", Primary: primary, Secondary: secondary,
		})
	}

	// Extra limit families: any x-<id>-primary-used-percent. The default
	// family is already represented above.
	const suffix = "-primary-used-percent"
	seen := map[string]struct{}{"codex": {}}
	for key := range h {
		lower := strings.ToLower(key)
		if !strings.HasSuffix(lower, suffix) || !strings.HasPrefix(lower, "x-") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(lower, "x-"), suffix)
		if id == "" || len(seen) >= maxRateLimitFamilies {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		fam := RateLimitFamily{
			ID:        id,
			Name:      headerString(h, "x-"+id+"-limit-name"),
			Primary:   windowFromHeaders(h, "x-"+id+"-primary"),
			Secondary: windowFromHeaders(h, "x-"+id+"-secondary"),
		}
		if fam.Name == "" {
			fam.Name = id
		}
		rl.Families = append(rl.Families, fam)
	}

	rl.Credits = creditsFromHeaders(h)
	if !hasUsedPercent(rl) && rl.Credits == nil {
		return RateLimits{}, false
	}
	return rl, true
}

func windowFromHeaders(h http.Header, prefix string) *RateLimitWindow {
	used, hasUsed := headerF64(h, prefix+"-used-percent")
	minutes := headerI64(h, prefix+"-window-minutes")
	reset := headerI64(h, prefix+"-reset-at")
	if !hasUsed && minutes == 0 && reset == 0 {
		return nil
	}
	w := &RateLimitWindow{WindowMinutes: minutes, ResetAt: reset}
	if hasUsed {
		w.UsedPct = &used
	}
	return w
}

func creditsFromHeaders(h http.Header) *RateLimitCredits {
	has, hasOK := headerBool(h, "x-codex-credits-has-credits")
	unlimited, unlimitedOK := headerBool(h, "x-codex-credits-unlimited")
	raw := h.Get("x-codex-credits-balance")
	if !hasOK && !unlimitedOK && raw == "" {
		return nil
	}
	credits := &RateLimitCredits{
		Present:    true,
		HasCredits: has,
		Unlimited:  unlimited,
	}
	if raw != "" {
		credits.Balance, credits.Raw = parseCreditsBalance(raw)
	}
	return credits
}

func hasUsedPercent(rl RateLimits) bool {
	for _, fam := range rl.Families {
		if fam.Primary != nil && fam.Primary.UsedPct != nil {
			return true
		}
		if fam.Secondary != nil && fam.Secondary.UsedPct != nil {
			return true
		}
	}
	return false
}

func parseCreditsBalance(raw string) (*float64, string) {
	cleaned := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "$"))
	cleaned = strings.ReplaceAll(cleaned, ",", "")
	if f, err := strconv.ParseFloat(cleaned, 64); err == nil {
		return &f, ""
	}
	return nil, raw
}

func headerF64(h http.Header, name string) (float64, bool) {
	v := h.Get(name)
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

func headerI64(h http.Header, name string) int64 {
	v := h.Get(name)
	if v == "" {
		return 0
	}
	i, err := strconv.ParseInt(v, 10, 64)
	if err != nil || i < 0 {
		return 0
	}
	return i
}

func headerBool(h http.Header, name string) (bool, bool) {
	v := h.Get(name)
	if v == "" {
		return false, false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, false
	}
	return b, true
}

func headerString(h http.Header, name string) string {
	return strings.TrimSpace(h.Get(name))
}

// sseRateLimitTap is an io.Writer for io.TeeReader: it forwards the stream
// untouched and parses codex.rate_limits SSE events off the passing bytes.
type sseRateLimitTap struct {
	sink func(RateLimits)
	mu   sync.Mutex
	buf  string
	data string
}

func newSSERateLimitTap(sink func(RateLimits)) *sseRateLimitTap {
	return &sseRateLimitTap{sink: sink}
}

func (t *sseRateLimitTap) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf += string(p)
	for {
		i := strings.IndexByte(t.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(t.buf[:i], "\r")
		t.buf = t.buf[i+1:]
		if line == "" {
			t.flush()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			t.data += strings.TrimSpace(strings.TrimPrefix(line, "data:")) + "\n"
		}
	}
	return len(p), nil
}

func (t *sseRateLimitTap) flush() {
	payload := strings.TrimSpace(t.data)
	t.data = ""
	if payload == "" || !strings.Contains(payload, "rate_limits") {
		return
	}
	var event struct {
		RateLimits *struct {
			Primary   *sseRateLimitWindow `json:"primary"`
			Secondary *sseRateLimitWindow `json:"secondary"`
			Credits   *struct {
				HasCredits bool            `json:"has_credits"`
				Unlimited  bool            `json:"unlimited"`
				Balance    json.RawMessage `json:"balance"`
			} `json:"credits"`
			MeteredLimitName string `json:"metered_limit_name"`
			LimitName        string `json:"limit_name"`
		} `json:"rate_limits"`
	}
	if err := json.Unmarshal([]byte(payload), &event); err != nil || event.RateLimits == nil {
		return
	}
	name := event.RateLimits.MeteredLimitName
	if name == "" {
		name = event.RateLimits.LimitName
	}
	if name == "" {
		name = "codex"
	}
	rl := RateLimits{Source: "codex_sse"}
	fam := RateLimitFamily{ID: name, Name: name}
	if w := rateLimitEventWindow(event.RateLimits.Primary); w != nil {
		fam.Primary = w
		rl.PrimaryUsedPct = w.UsedPct
		rl.PrimaryResetAt = w.ResetAt
		rl.PrimaryWindowMinutes = w.WindowMinutes
	}
	if w := rateLimitEventWindow(event.RateLimits.Secondary); w != nil {
		fam.Secondary = w
		rl.SecondaryUsedPct = w.UsedPct
		rl.SecondaryResetAt = w.ResetAt
		rl.SecondaryWindowMinutes = w.WindowMinutes
	}
	if fam.Primary != nil || fam.Secondary != nil {
		rl.Families = append(rl.Families, fam)
	}
	if c := event.RateLimits.Credits; c != nil {
		credits := &RateLimitCredits{Present: true, HasCredits: c.HasCredits, Unlimited: c.Unlimited}
		if len(c.Balance) > 0 {
			credits.Balance, credits.Raw = parseJSONCreditsBalance(c.Balance)
		}
		rl.Credits = credits
	}
	if (len(rl.Families) == 0 && rl.Credits == nil) || (!hasUsedPercent(rl) && rl.Credits == nil) {
		return
	}
	if t.sink != nil {
		t.sink(rl)
	}
}

type sseRateLimitWindow struct {
	UsedPercent   *float64 `json:"used_percent"`
	WindowMinutes int64    `json:"window_minutes"`
	ResetAt       int64    `json:"reset_at"`
}

func rateLimitEventWindow(w *sseRateLimitWindow) *RateLimitWindow {
	if w == nil {
		return nil
	}
	out := &RateLimitWindow{UsedPct: w.UsedPercent, WindowMinutes: w.WindowMinutes, ResetAt: w.ResetAt}
	if out.UsedPct == nil && out.WindowMinutes == 0 && out.ResetAt == 0 {
		return nil
	}
	return out
}

func parseJSONCreditsBalance(raw json.RawMessage) (*float64, string) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return parseCreditsBalance(s)
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return &f, ""
	}
	return nil, string(raw)
}
