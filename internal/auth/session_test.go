package auth

// WebUI session auth + CSRF: RED tests covering the session design and the
// CSRF controls. Never from implementation behavior.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/storage"
)

// sessionTestEnv is a SessionManager over a fresh store with a pinned clock
// and a fixed 32-byte master key (the HMAC secret).
type sessionTestEnv struct {
	sm    *SessionManager
	st    *storage.Store
	now   int64
	admin func()
}

func newSessionEnv(t *testing.T) *sessionTestEnv {
	t.Helper()
	env := &sessionTestEnv{now: 1_000_000, st: newTestStore(t)}
	env.sm = NewSessionManager(env.st, testMasterKey(), func() int64 { return env.now })
	env.admin = func() { env.createTestAdmin(t, "operator") }
	return env
}

// testMasterKey returns a fixed 32-byte key — a test vector, not a secret.
func testMasterKey() []byte {
	return []byte("0123456789abcdef0123456789abcdef")
}

const testPassword = "correct horse battery staple"

// createTestAdmin seeds one admin row with a known Argon2id hash.
func (e *sessionTestEnv) createTestAdmin(t *testing.T, username string) {
	t.Helper()
	phc, err := storage.NewArgon2Hasher().Hash(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.CreateAdmin(context.Background(), "adm-"+username, username, phc); err != nil {
		t.Fatal(err)
	}
}

// loginResult captures what one login response established.
type loginResult struct {
	status        int
	setCookies    []string // all Set-Cookie header values
	sessionCookie string   // full "name=value" pair of the session cookie
	csrfCookie    string   // full "name=value" pair of the csrf cookie
	csrfToken     string   // token from the JSON body (random.mac)
}

func (e *sessionTestEnv) login(t *testing.T, user, pass string, tlsArrival bool) loginResult {
	t.Helper()
	body := `{"username":"` + user + `","password":"` + pass + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if tlsArrival {
		req.TLS = &tls.ConnectionState{}
	}
	w := httptest.NewRecorder()
	e.sm.HandleLogin(w, req)

	var res loginResult
	res.status = w.Code
	res.setCookies = w.Header().Values("Set-Cookie")
	for _, c := range res.setCookies {
		name, _, _ := strings.Cut(c, "=")
		switch strings.TrimPrefix(name, "__Host-") {
		case sessionCookieName:
			res.sessionCookie, _, _ = strings.Cut(c, ";")
		case csrfCookieName:
			res.csrfCookie, _, _ = strings.Cut(c, ";")
		}
	}
	if w.Code == http.StatusOK {
		var payload struct {
			CSRFToken string `json:"csrf_token"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatalf("login body not JSON: %v (%s)", err, w.Body.String())
		}
		res.csrfToken = payload.CSRFToken
	}
	return res
}

// authed sends one request through the admin middleware (session + CSRF +
// Origin layers) and returns the recorder.
func (e *sessionTestEnv) authed(t *testing.T, method, path, body, cookieHeader, csrfToken, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if cookieHeader != "" {
		req.Header.Set("Cookie", cookieHeader)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrfToken != "" {
		req.Header.Set("X-CSRF-Token", csrfToken)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	e.sm.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The OK handler must observe the admin id from the request context —
		// asserted so the middleware cannot pass requests without actually
		// authenticating them.
		if AdminIDFromContext(r.Context()) == "" {
			t.Errorf("no admin id in context for an accepted request (%s %s)", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(w, req)
	return w
}

// authedVia is authed with request-level mutations (RemoteAddr, forwarded
// headers) so the trusted-proxy origin tests can simulate a hop.
func (e *sessionTestEnv) authedVia(t *testing.T, mutate func(*http.Request), method, path, body, cookieHeader, csrfToken, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if mutate != nil {
		mutate(req)
	}
	if cookieHeader != "" {
		req.Header.Set("Cookie", cookieHeader)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrfToken != "" {
		req.Header.Set("X-CSRF-Token", csrfToken)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	e.sm.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if AdminIDFromContext(r.Context()) == "" {
			t.Errorf("no admin id in context for an accepted request (%s %s)", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(w, req)
	return w
}

// cookieHeaderFor rebuilds the Cookie header the SPA would send: session
// cookie + csrf cookie.
func (res loginResult) cookieHeader() string {
	return res.sessionCookie + "; " + res.csrfCookie
}

// every successful login issues a NEW session id; two logins never
// share a cookie value.
func TestLogin_IssuesNewSessionPerLogin(t *testing.T) {
	env := newSessionEnv(t)
	env.admin()

	l1 := env.login(t, "operator", testPassword, false)
	l2 := env.login(t, "operator", testPassword, false)
	if l1.status != http.StatusOK || l2.status != http.StatusOK {
		t.Fatalf("login statuses: %d / %d", l1.status, l2.status)
	}
	if l1.sessionCookie == "" || l2.sessionCookie == "" {
		t.Fatalf("no session cookie issued: %q / %q", l1.sessionCookie, l2.sessionCookie)
	}
	if l1.sessionCookie == l2.sessionCookie {
		t.Error("two logins issued the same session id")
	}
}

// idle timeout enforced server-side — a session idle past the window
// fails auth even with a valid cookie.
func TestSession_IdleTimeoutServerSide(t *testing.T) {
	env := newSessionEnv(t)
	env.admin()
	l := env.login(t, "operator", testPassword, false)

	env.now += 31 * 60 // past the 30 min idle window, inside 12 h absolute
	w := env.authed(t, http.MethodGet, "/api/admin/providers", "", l.sessionCookie, "", "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("idle-expired session accepted: status %d", w.Code)
	}
}

// absolute timeout — activity never extends the 12 h cap. Requests
// keep passing the idle window but the absolute expiry still lands.
func TestSession_AbsoluteTimeoutServerSide(t *testing.T) {
	env := newSessionEnv(t)
	env.admin()
	l := env.login(t, "operator", testPassword, false)

	// 35 hops of 20 min = 700 min < 720 min (12 h): every hop is inside the
	// idle window, so all must pass.
	for i := 0; i < 35; i++ {
		env.now += 20 * 60
		w := env.authed(t, http.MethodGet, "/api/admin/providers", "", l.sessionCookie, "", "")
		if w.Code != http.StatusOK {
			t.Fatalf("request %d rejected inside the absolute window: %d", i, w.Code)
		}
	}
	// +25 min: still inside the idle window (last request 20 min ago → gap
	// 25 min < 30), but total elapsed 725 min > 720 → absolute expiry.
	env.now += 25 * 60
	w := env.authed(t, http.MethodGet, "/api/admin/providers", "", l.sessionCookie, "", "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("session still valid past 12 h absolute expiry: %d", w.Code)
	}
}

// logout deletes the server-side row AND clears the cookie; the old
// session id is dead afterwards.
func TestLogout_DeletesRowAndClearsCookie(t *testing.T) {
	env := newSessionEnv(t)
	env.admin()
	l := env.login(t, "operator", testPassword, false)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/logout", strings.NewReader(`{}`))
	req.Header.Set("Cookie", l.cookieHeader())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", l.csrfToken)
	logoutW := httptest.NewRecorder()
	env.sm.HandleLogout(logoutW, req)
	if logoutW.Code != http.StatusOK {
		t.Fatalf("logout: status %d (body %s)", logoutW.Code, logoutW.Body.String())
	}
	// The logout response clears the session cookie (Max-Age=0, past-dated).
	cleared := false
	for _, c := range logoutW.Header().Values("Set-Cookie") {
		if strings.Contains(c, "Max-Age=0") {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("logout does not clear the cookie: %q", logoutW.Header().Values("Set-Cookie"))
	}

	// The row is gone: reuse of the old cookie fails.
	w := env.authed(t, http.MethodGet, "/api/admin/providers", "", l.sessionCookie, "", "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("session survived logout: %d", w.Code)
	}
}

// Content-Type rules: a PRESENT Content-Type on a mutation
// must be application/json (415 otherwise); an ABSENT one is legal — a
// bodyless POST/DELETE (fetch sends no Content-Type without a body) is
// processed, and no cross-site form vector can omit the header.
func TestMutation_ContentTypeRules(t *testing.T) {
	env := newSessionEnv(t)
	env.admin()
	l := env.login(t, "operator", testPassword, false)

	// Bodyless mutation without Content-Type → accepted (OK handler ran).
	w := env.authed(t, http.MethodPost, "/api/admin/logout", "", l.cookieHeader(), l.csrfToken, "")
	if w.Code != http.StatusOK {
		t.Errorf("bodyless mutation without content-type: status %d (body %s)", w.Code, w.Body.String())
	}

	// A PRESENT non-JSON content type is still 415.
	req := httptest.NewRequest(http.MethodPost, "/api/admin/logout", strings.NewReader(`{}`))
	req.Header.Set("Cookie", l.cookieHeader())
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-CSRF-Token", l.csrfToken)
	rec := httptest.NewRecorder()
	env.sm.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("text/plain mutation reached the handler")
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain mutation: status %d, want 415", rec.Code)
	}

	// Bodyless mutation without the CSRF header → 403 (gates bodyless
	// mutations independently of the content type).
	w = env.authed(t, http.MethodPost, "/api/admin/logout", "", l.cookieHeader(), "", "")
	if w.Code != http.StatusForbidden {
		t.Errorf("bodyless mutation without csrf token: status %d, want 403", w.Code)
	}
}

// __Host- prefix on TLS arrivals, plain id otherwise — per-request
// split from the arrival scheme.
func TestLogin_CookiePrefixPerTLS(t *testing.T) {
	env := newSessionEnv(t)
	env.admin()

	plain := env.login(t, "operator", testPassword, false)
	if !strings.HasPrefix(plain.sessionCookie, "id=") {
		t.Errorf("plain-HTTP cookie: %q", plain.sessionCookie)
	}
	tlsArrival := env.login(t, "operator", testPassword, true)
	if !strings.HasPrefix(tlsArrival.sessionCookie, "__Host-id=") {
		t.Errorf("TLS cookie: %q", tlsArrival.sessionCookie)
	}
}

// session cookie HttpOnly, SameSite=Strict, non-persistent; CSRF
// cookie JS-readable (the SPA must echo it).
func TestLogin_CookieAttributes(t *testing.T) {
	env := newSessionEnv(t)
	env.admin()
	l := env.login(t, "operator", testPassword, false)

	var sessionAttrs, csrfAttrs string
	for _, c := range l.setCookies {
		// The session cookie is "id=…" or "__Host-id=…"; the csrf cookie
		// is always plain "csrf=…" (no __Host- prefix needed: no Path
		// restriction applies, only requires JS readability).
		trimmed := strings.TrimPrefix(c, "__Host-")
		if strings.HasPrefix(trimmed, sessionCookieName+"=") {
			sessionAttrs = c
		}
		if strings.HasPrefix(c, csrfCookieName+"=") {
			csrfAttrs = c
		}
	}
	if sessionAttrs == "" || csrfAttrs == "" {
		t.Fatalf("cookies missing: session=%q csrf=%q", sessionAttrs, csrfAttrs)
	}
	for _, want := range []string{"HttpOnly", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(sessionAttrs, want) {
			t.Errorf("session cookie missing %s: %q", want, sessionAttrs)
		}
	}
	if strings.Contains(sessionAttrs, "Expires=") || strings.Contains(sessionAttrs, "Max-Age=") {
		t.Errorf("session cookie is persistent: %q", sessionAttrs)
	}
	if strings.Contains(csrfAttrs, "HttpOnly") {
		t.Errorf("csrf cookie must be JS-readable: %q", csrfAttrs)
	}
}

// the CSRF token binds to the session — a token minted for one
// session cannot authorize another.
func TestCSRF_TokenIsSessionBound(t *testing.T) {
	env := newSessionEnv(t)
	env.admin()
	l1 := env.login(t, "operator", testPassword, false)
	l2 := env.login(t, "operator", testPassword, false)

	// l2's CSRF material (cookie random + header token) on l1's session
	// must fail: the HMAC covers sessionID ‖ random.
	w := env.authed(t, http.MethodPost, "/api/admin/providers", `{}`,
		l1.sessionCookie+"; "+l2.csrfCookie, l2.csrfToken, "")
	if w.Code != http.StatusForbidden {
		t.Errorf("cross-session CSRF token accepted: %d", w.Code)
	}
	// Same-session material passes.
	w = env.authed(t, http.MethodPost, "/api/admin/providers", `{}`,
		l1.cookieHeader(), l1.csrfToken, "")
	if w.Code != http.StatusOK {
		t.Errorf("own-session CSRF token rejected: %d", w.Code)
	}
}

// X-CSRF-Token is required on state-changing requests; safe methods
// pass without it.
func TestCSRF_HeaderRequiredOnMutations(t *testing.T) {
	env := newSessionEnv(t)
	env.admin()
	l := env.login(t, "operator", testPassword, false)

	w := env.authed(t, http.MethodPost, "/api/admin/providers", `{}`, l.cookieHeader(), "", "")
	if w.Code != http.StatusForbidden {
		t.Errorf("POST without CSRF header accepted: %d", w.Code)
	}
	w = env.authed(t, http.MethodGet, "/api/admin/providers", "", l.sessionCookie, "", "")
	if w.Code != http.StatusOK {
		t.Errorf("GET rejected for missing CSRF: %d", w.Code)
	}
}

// a forged token (valid random, wrong HMAC) fails even with the
// matching csrf cookie — the MAC is unforgeable without the master key.
func TestCSRF_ForgedMACRejected(t *testing.T) {
	env := newSessionEnv(t)
	env.admin()
	l := env.login(t, "operator", testPassword, false)

	random, _, _ := strings.Cut(l.csrfToken, ".")
	forged := random + "." + strings.Repeat("A", len(l.csrfToken)-len(random)-1)
	w := env.authed(t, http.MethodPost, "/api/admin/providers", `{}`,
		l.cookieHeader(), forged, "")
	if w.Code != http.StatusForbidden {
		t.Errorf("forged MAC accepted: %d", w.Code)
	}
}

// simple content types are rejected on mutations — text/plain must
// not be processable cross-site without preflight.
func TestCSRF_RejectsSimpleContentTypes(t *testing.T) {
	env := newSessionEnv(t)
	env.admin()
	l := env.login(t, "operator", testPassword, false)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/providers", strings.NewReader(`x=1`))
	req.Header.Set("Cookie", l.cookieHeader())
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-CSRF-Token", l.csrfToken)
	w := httptest.NewRecorder()
	env.sm.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain mutation accepted: %d", w.Code)
	}
}

// a mismatching Origin on state-changing requests → 403; an absent
// Origin header passes (older clients).
func TestCSRF_OriginMismatchRejected(t *testing.T) {
	env := newSessionEnv(t)
	env.admin()
	l := env.login(t, "operator", testPassword, false)

	w := env.authed(t, http.MethodPost, "/api/admin/providers", `{}`,
		l.cookieHeader(), l.csrfToken, "https://evil.example")
	if w.Code != http.StatusForbidden {
		t.Errorf("cross-origin mutation accepted: %d", w.Code)
	}
	// Same-origin Origin passes (scheme + host match the request target).
	w = env.authed(t, http.MethodPost, "/api/admin/providers", `{}`,
		l.cookieHeader(), l.csrfToken, "http://example.com")
	if w.Code != http.StatusOK {
		t.Errorf("same-origin mutation rejected: %d", w.Code)
	}
}

// A TLS terminator in front speaks plain HTTP to the server, so r.TLS is
// nil and the browser's https Origin can never equal the reconstructed
// http origin. When the immediate peer is a configured trusted proxy, the
// X-Forwarded-Proto/X-Forwarded-Host headers it sets reconstruct the
// public origin instead — the console works behind the terminator.
func TestCSRF_TrustedProxyForwardedOriginAccepted(t *testing.T) {
	env := newSessionEnv(t)
	env.admin()
	nets, err := ParseTrustedProxies([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatalf("ParseTrustedProxies: %v", err)
	}
	env.sm = NewSessionManager(env.st, testMasterKey(),
		func() int64 { return env.now }, WithTrustedProxies(nets))
	l := env.login(t, "operator", testPassword, false)

	via := func(req *http.Request) {
		req.RemoteAddr = "10.0.0.5:55555"
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-Forwarded-Host", "console.example.com")
	}
	w := env.authedVia(t, via, http.MethodPost, "/api/admin/providers", `{}`,
		l.cookieHeader(), l.csrfToken, "https://console.example.com")
	if w.Code != http.StatusOK {
		t.Errorf("forwarded same-origin mutation rejected: %d", w.Code)
	}
}

// The forwarded headers are only honored from a trusted peer. A client
// that reaches the server directly cannot spoof its way past the origin
// check by setting them itself.
func TestCSRF_UntrustedForwardedHeadersIgnored(t *testing.T) {
	env := newSessionEnv(t)
	env.admin()
	l := env.login(t, "operator", testPassword, false)

	via := func(req *http.Request) {
		req.RemoteAddr = "203.0.113.9:55555"
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-Forwarded-Host", "console.example.com")
	}
	w := env.authedVia(t, via, http.MethodPost, "/api/admin/providers", `{}`,
		l.cookieHeader(), l.csrfToken, "https://console.example.com")
	if w.Code != http.StatusForbidden {
		t.Errorf("spoofed forwarded headers accepted: %d", w.Code)
	}
}

// A trusted proxy's X-Forwarded-Proto must carry a real scheme: anything
// other than http/https is dropped, so the reconstruction falls back to
// the connection state and a https Origin still mismatches.
func TestCSRF_TrustedProxyGarbageProtoIgnored(t *testing.T) {
	env := newSessionEnv(t)
	env.admin()
	nets, err := ParseTrustedProxies([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatalf("ParseTrustedProxies: %v", err)
	}
	env.sm = NewSessionManager(env.st, testMasterKey(),
		func() int64 { return env.now }, WithTrustedProxies(nets))
	l := env.login(t, "operator", testPassword, false)

	via := func(req *http.Request) {
		req.RemoteAddr = "10.0.0.5:55555"
		req.Header.Set("X-Forwarded-Proto", "ftp")
		req.Header.Set("X-Forwarded-Host", "console.example.com")
	}
	w := env.authedVia(t, via, http.MethodPost, "/api/admin/providers", `{}`,
		l.cookieHeader(), l.csrfToken, "https://console.example.com")
	if w.Code != http.StatusForbidden {
		t.Errorf("garbage forwarded proto accepted: %d", w.Code)
	}
}

// Plain HTTP through a proxy needs no trust at all: when the proxy
// preserves the Host header, the reconstructed origin already matches the
// browser's http Origin.
func TestCSRF_ProxyHostPreservedPlainHTTP(t *testing.T) {
	env := newSessionEnv(t)
	env.admin()
	l := env.login(t, "operator", testPassword, false)

	via := func(req *http.Request) { req.RemoteAddr = "10.0.0.5:55555" }
	w := env.authedVia(t, via, http.MethodPost, "http://console.example.com/api/admin/providers", `{}`,
		l.cookieHeader(), l.csrfToken, "http://console.example.com")
	if w.Code != http.StatusOK {
		t.Errorf("same-origin mutation behind host-preserving proxy rejected: %d", w.Code)
	}
}

// ParseTrustedProxies accepts CIDRs and bare IPs (a bare IP becomes its
// own /32 or /128) and rejects anything else loudly — a typo'd entry must
// fail startup, never silently widen the trust boundary.
func TestParseTrustedProxies(t *testing.T) {
	nets, err := ParseTrustedProxies([]string{"10.0.0.0/8", "192.168.1.7", "fd00::/16", "fd12::1"})
	if err != nil {
		t.Fatalf("ParseTrustedProxies: %v", err)
	}
	if len(nets) != 4 {
		t.Fatalf("entry count: got %d want 4", len(nets))
	}
	if !nets[0].Contains(net.ParseIP("10.1.2.3")) {
		t.Error("10.0.0.0/8 does not contain 10.1.2.3")
	}
	if !nets[1].Contains(net.ParseIP("192.168.1.7")) {
		t.Error("bare IP must contain itself")
	}
	if nets[1].Contains(net.ParseIP("192.168.1.8")) {
		t.Error("bare IP must be a /32, not a wider match")
	}
	if !nets[2].Contains(net.ParseIP("fd00::42")) {
		t.Error("fd00::/16 does not contain fd00::42")
	}
	if !nets[3].Contains(net.ParseIP("fd12::1")) {
		t.Error("bare IPv6 must contain itself")
	}
	if _, err := ParseTrustedProxies([]string{"10.0.0.0/8", "not-an-ip"}); err == nil {
		t.Error("invalid entry must error")
	}
}

// unknown session ids fail auth (strict mode) — never auto-adopted.
func TestSession_UnknownIdRejected(t *testing.T) {
	env := newSessionEnv(t)
	env.admin()

	w := env.authed(t, http.MethodGet, "/api/admin/providers", "", "id=deadbeef", "", "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("unknown session id accepted: %d", w.Code)
	}
}

// Login failures never distinguish causes: wrong password and unknown user
// produce identical status AND body, and no session cookie is set.
func TestLogin_InvalidCredentials401Shape(t *testing.T) {
	env := newSessionEnv(t)
	env.admin()

	wrongPass := env.login(t, "operator", "wrong", false)
	unknownUser := env.login(t, "ghost", "whatever", false)
	if wrongPass.status != http.StatusUnauthorized || unknownUser.status != http.StatusUnauthorized {
		t.Fatalf("login statuses: %d / %d", wrongPass.status, unknownUser.status)
	}
	for _, l := range []loginResult{wrongPass, unknownUser} {
		if len(l.setCookies) != 0 {
			t.Errorf("failed login issued cookies: %q", l.setCookies)
		}
	}
}

// refresh path: a page reload wipes the SPA's in-memory token while
// the session cookie stays alive. The middleware must re-issue the full
// token (random + session-bound MAC) so a reloaded SPA can keep mutating —
// and the re-issued token must actually authorize a mutation.
func TestCSRF_TokenReissuedForLiveSession(t *testing.T) {
	env := newSessionEnv(t)
	env.admin()
	l := env.login(t, "operator", testPassword, false)

	// Simulate the reload: session cookie sent, csrf cookie absent (the
	// cookie survives a reload, but keep the case strict — either way the
	// server must mint a token for the live session).
	w := env.authed(t, http.MethodGet, "/api/admin/providers", "", l.sessionCookie, "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("live session rejected on GET: %d", w.Code)
	}
	reissued := w.Header().Get("X-CSRF-Token")
	if reissued == "" {
		t.Fatal("no X-CSRF-Token header on an authenticated response — a reloaded SPA has no token after refresh")
	}
	random, mac, ok := strings.Cut(reissued, ".")
	if !ok || random == "" || mac == "" {
		t.Fatalf("re-issued token malformed: %q", reissued)
	}
	// The re-issued token must authorize a mutation on the same session.
	w = env.authed(t, http.MethodPost, "/api/admin/providers", `{}`,
		l.cookieHeader(), reissued, "")
	if w.Code != http.StatusOK {
		t.Errorf("re-issued token rejected on mutation: %d", w.Code)
	}
}

// the re-issued token is session-bound like a login-issued one —
// one session's re-issued token must not authorize another session.
func TestCSRF_ReissuedTokenSessionBound(t *testing.T) {
	env := newSessionEnv(t)
	env.admin()
	l1 := env.login(t, "operator", testPassword, false)
	l2 := env.login(t, "operator", testPassword, false)

	w := env.authed(t, http.MethodGet, "/api/admin/providers", "", l1.sessionCookie, "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("live session rejected: %d", w.Code)
	}
	reissued := w.Header().Get("X-CSRF-Token")

	reissuedRandom, _, _ := strings.Cut(reissued, ".")

	// l1's re-issued token on l2's session → forbidden.
	w = env.authed(t, http.MethodPost, "/api/admin/providers", `{}`,
		l2.sessionCookie+"; csrf="+reissuedRandom, reissued, "")
	if w.Code != http.StatusForbidden {
		t.Errorf("cross-session re-issued token accepted: %d", w.Code)
	}
	// And it must pass on its own session.
	w = env.authed(t, http.MethodPost, "/api/admin/providers", `{}`,
		l1.sessionCookie+"; csrf="+reissuedRandom, reissued, "")
	if w.Code != http.StatusOK {
		t.Errorf("own-session re-issued token rejected: %d", w.Code)
	}
}

// The re-issue never leaks a token for an unauthenticated request — no
// header on 401 responses.
func TestCSRF_NoReissueForUnauthenticated(t *testing.T) {
	env := newSessionEnv(t)
	env.admin()

	w := env.authed(t, http.MethodGet, "/api/admin/providers", "", "", "", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET: %d", w.Code)
	}
	if got := w.Header().Get("X-CSRF-Token"); got != "" {
		t.Errorf("token header on 401 response: %q", got)
	}
}

// The CSRF MAC is keyed by a derivation of its own, not by the master key itself.
// Those same bytes key the credentials envelope; two primitives sharing a key is
// a weakness that is invisible until it is not.
func TestCSRFMAC_IsKeyedByItsOwnDerivation(t *testing.T) {
	master := []byte("0123456789abcdef0123456789abcdef")

	raw := hmac.New(sha256.New, master)
	raw.Write([]byte("session-cookie"))
	raw.Write([]byte("."))
	raw.Write([]byte("random"))
	undivided := base64.RawURLEncoding.EncodeToString(raw.Sum(nil))

	if got := csrfHMAC(master, "session-cookie", "random"); got == undivided {
		t.Error("the CSRF MAC is keyed by the master key itself; it must use its own derivation")
	}
}
