package auth

// WebUI session auth + CSRF middleware, implementing the session and CSRF
// contract for the admin surface:
//   - 256-bit CSPRNG session IDs, SHA-256-hashed at rest
//   - generic cookie name `id`, `__Host-id` when the request
//     arrived over TLS (per-request split)
//   - HttpOnly, non-persistent, SameSite=Strict
//   - strict mode — unknown IDs are rejected, never adopted
//   - all session state server-side; the raw ID lives only in the cookie
//   - a fresh session per login (renewal)
//   - 30 min idle / 12 h absolute, enforced against DB timestamps
//   - logout = row delete + cookie clear
//   - CSRF token = random + HMAC-SHA-256(master key, sessionID‖random)
//   - X-CSRF-Token required on mutations; a PRESENT Content-Type on a
//     mutation must be application/json
//   - simple content types on mutations → 415 (an ABSENT one is legal:
//     bodyless mutations — DELETEs, the oauth start/poll POSTs — carry no
//     header, and no cross-site vector can omit it; a form post always
//     carries urlencoded/text/plain, which still 415)
//   - Origin (when present) must match scheme+host exactly → 403
//
// Failure responses never distinguish causes (the 401 bodies are
// uniform) and upstream/credential material is never echoed.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/tubeof/proxyllm/internal/storage"
)

const (
	// sessionCookieName is deliberately generic: it reveals nothing
	// about the software in front of the proxy.
	sessionCookieName = "id"
	// csrfCookieName carries only the random half of the CSRF token;
	// the full token is returned to the SPA at login and echoed via the
	// X-CSRF-Token header. JS-readable — NOT HttpOnly.
	csrfCookieName = "csrf"

	// windows, in seconds, enforced server-side against store timestamps.
	sessionIdleWindow  = 30 * 60
	sessionAbsoluteCap = 12 * 60 * 60

	// 256-bit session IDs. 128-bit CSRF randomness is plenty —
	// the MAC carries the security, the random only binds cookie↔header.
	sessionIDBytes  = 32
	csrfRandomBytes = 16
)

// dummyArgon2PHC is a well-formed Argon2id PHC string at OWASP-minimum
// parameters whose digest matches nothing. Unknown usernames run a verify
// against it so login timing cannot distinguish "no such user" from "wrong
// password" (the 401 shape is uniform — body AND observable behavior).
const dummyArgon2PHC = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA"

// SessionManager owns the WebUI login/logout handlers and the admin
// middleware. now is injectable so the idle/absolute windows are testable
// against pinned clocks.
type SessionManager struct {
	st             *storage.Store
	masterKey      []byte
	now            func() int64
	trustedProxies []*net.IPNet
}

// SessionOption configures NewSessionManager (a separate type from the
// client-key middleware's Option).
type SessionOption func(*SessionManager)

// WithTrustedProxies sets the reverse proxies (ParseTrustedProxies output)
// whose forwarded headers the origin reconstruction honors. Nil/empty keeps
// the direct-connection behavior: X-Forwarded-* is never trusted then.
func WithTrustedProxies(nets []*net.IPNet) SessionOption {
	return func(sm *SessionManager) { sm.trustedProxies = nets }
}

// NewSessionManager wires a manager over st with the CSRF master key
// (32 bytes recommended; used only as an HMAC secret) and a clock.
func NewSessionManager(st *storage.Store, masterKey []byte, now func() int64, opts ...SessionOption) *SessionManager {
	if now == nil {
		now = func() int64 { return time.Now().Unix() }
	}
	sm := &SessionManager{st: st, masterKey: masterKey, now: now}
	for _, opt := range opts {
		opt(sm)
	}
	return sm
}

type adminCtxKey struct{}

// AdminIDFromContext returns the authenticated admin id for requests that
// passed RequireAdmin ("" otherwise). Handlers use it for audit entries —
// never for authorization decisions of their own.
func AdminIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(adminCtxKey{}).(string)
	return id
}

type sessionIDHashCtxKey struct{}

// SessionIDHashFromContext returns the hashed session id of the request
// that passed RequireAdmin ("" otherwise). The password-change handler
// uses it to keep the requesting session while revoking the admin's
// others — the hash never leaves the server process except here.
func SessionIDHashFromContext(ctx context.Context) string {
	h, _ := ctx.Value(sessionIDHashCtxKey{}).(string)
	return h
}

type rawSessionCtxKey struct{}

// rawSessionIDFromContext exposes the raw (unhashed) session id to the
// logout handler; it never leaves the auth package.
func rawSessionIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(rawSessionCtxKey{}).(string)
	return id
}

// HandleLogin authenticates an admin (Argon2id) and issues a brand-new
// session plus the CSRF pair. Failures are uniform 401s.
func (sm *SessionManager) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAdminError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10) // bounded input
	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid request")
		return
	}

	admin, err := sm.st.GetAdminByUsername(r.Context(), creds.Username)
	if errors.Is(err, storage.ErrNotFound) {
		// Equalize timing with the password-verify path.
		passwordHasher().Verify(dummyArgon2PHC, creds.Password)
		writeAdminUnauthorized(w)
		return
	}
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !passwordHasher().Verify(admin.PasswordHash, creds.Password) {
		writeAdminUnauthorized(w)
		return
	}

	rawID := make([]byte, sessionIDBytes)
	if _, err := rand.Read(rawID); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "internal error")
		return
	}
	now := sm.now()
	if err := sm.st.InsertSession(r.Context(), storage.Session{
		IDHash:            hashSessionID(rawIDEncoded(rawID)),
		AdminID:           admin.ID,
		CreatedAt:         now,
		LastSeenAt:        now,
		AbsoluteExpiresAt: now + sessionAbsoluteCap,
	}); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "internal error")
		return
	}

	random := make([]byte, csrfRandomBytes)
	if _, err := rand.Read(random); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "internal error")
		return
	}
	randomStr := base64.RawURLEncoding.EncodeToString(random)
	cookieValue := rawIDEncoded(rawID)
	token := randomStr + "." + csrfHMAC(sm.masterKey, cookieValue, randomStr)

	tlsArrival := r.TLS != nil
	setSessionCookie(w, cookieValue, tlsArrival)
	setCSRFCookie(w, randomStr, tlsArrival)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"csrf_token": token})
}

// HandleLogout runs the full admin auth layers (a logout IS a mutation —
// the CSRF checks apply), then deletes the session row and clears
// both cookies.
func (sm *SessionManager) HandleLogout(w http.ResponseWriter, r *http.Request) {
	sm.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := rawSessionIDFromContext(r.Context())
		if raw != "" {
			_ = sm.st.DeleteSession(r.Context(), hashSessionID(raw))
		}
		clearSessionCookie(w, r.TLS != nil)
		clearCSRFCookie(w)
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(w, r)
}

// SessionExpired reports whether a session is outside either server-side
// window (idle or absolute). The admin log stream re-checks this on every
// heartbeat so an already-open SSE connection cannot outlive logout,
// expiry or a password-change revocation.
func SessionExpired(sess *storage.Session, now int64) bool {
	return now-sess.LastSeenAt > sessionIdleWindow || now >= sess.AbsoluteExpiresAt
}

// RequireAdmin wraps admin handlers with session auth (strict,
// server-side windows) and, for state-changing methods, the CSRF layers:
// application/json only, Origin match when present, valid
// session-bound X-CSRF-Token.
func (sm *SessionManager) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := sessionCookieValue(r)
		if raw == "" {
			writeAdminUnauthorized(w)
			return
		}
		idHash := hashSessionID(raw)
		sess, err := sm.st.GetSession(r.Context(), idHash)
		if errors.Is(err, storage.ErrNotFound) {
			writeAdminUnauthorized(w)
			return
		}
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "internal error")
			return
		}
		now := sm.now()
		// both windows checked server-side against store timestamps —
		// a doctored client clock is irrelevant.
		if SessionExpired(sess, now) {
			writeAdminUnauthorized(w)
			return
		}
		if err := sm.st.TouchSession(r.Context(), idHash, now); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// Token re-issue: the SPA holds the CSRF token only in
		// memory, so a page refresh loses it while the session lives on.
		// The MAC needs the master key — the SPA cannot rebuild the token
		// from the cookie's random half. Every authenticated response
		// therefore carries the current token as a header, letting the SPA
		// pick it back up on any request (login/bootstrap bodies still carry
		// it for the initial acquisition).
		random, err := csrfRandomFor(w, r)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "internal error")
			return
		}
		w.Header().Set("X-CSRF-Token", random+"."+csrfHMAC(sm.masterKey, raw, random))

		if isMutation(r.Method) {
			// reject simple content types — a cross-site form or
			// text/plain fetch must never be processable without preflight.
			// An ABSENT header is legal: bodyless mutations (DELETEs, the
			// oauth start/poll POSTs) carry none, and fetch does not set a
			// default — while every cross-site form vector always sends one.
			// The CSRF token check below gates those independently.
			if ct := r.Header.Get("Content-Type"); ct != "" {
				mt, _, err := mime.ParseMediaType(ct)
				if err != nil || mt != "application/json" {
					writeAdminError(w, http.StatusUnsupportedMediaType, "content type must be application/json")
					return
				}
			}
			// when Origin is present it must match scheme+host exactly.
			if origin := r.Header.Get("Origin"); origin != "" && origin != sm.requestOrigin(r) {
				writeAdminError(w, http.StatusForbidden, "origin mismatch")
				return
			}
			// the token is checked regardless of SameSite protection.
			header := r.Header.Get("X-CSRF-Token")
			random, mac, ok := strings.Cut(header, ".")
			if !ok || !csrfConstantEqual(
				csrfHMAC(sm.masterKey, raw, random), mac) {
				writeAdminError(w, http.StatusForbidden, "csrf check failed")
				return
			}
		}

		ctx := context.WithValue(r.Context(), adminCtxKey{}, sess.AdminID)
		ctx = context.WithValue(ctx, sessionIDHashCtxKey{}, idHash)
		ctx = context.WithValue(ctx, rawSessionCtxKey{}, raw)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// csrfRandomFor returns the CSRF random half for this session: the csrf
// cookie's value when present (the common case — token stability across
// requests), otherwise a freshly minted one, set as a replacement cookie.
// The random alone authorizes nothing (the MAC carries the security);
// it only binds cookie and header.
func csrfRandomFor(w http.ResponseWriter, r *http.Request) (string, error) {
	if c, err := r.Cookie(csrfCookieName); err == nil && c.Value != "" {
		return c.Value, nil
	}
	random := make([]byte, csrfRandomBytes)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	randomStr := base64.RawURLEncoding.EncodeToString(random)
	setCSRFCookie(w, randomStr, r.TLS != nil)
	return randomStr, nil
}

// csrfDerivationLabel domain-separates this use of the master key from every
// other one. The same bytes already key the credentials envelope and the client
// key hasher; deriving a distinct key per use means a weakness in one of them can
// never be a weakness in another. The version rides the label, so a future
// rotation changes the label rather than the construction — every token minted
// before it simply stops verifying, which for an ephemeral token costs one page
// reload.
const csrfDerivationLabel = "proxyllm.csrf-mac/v1"

// csrfKey derives the CSRF MAC key. Derived once per call from a short secret,
// never stored: the derivation is two HMAC rounds over a fixed label.
func csrfKey(masterKey []byte) []byte {
	mac := hmac.New(sha256.New, masterKey)
	mac.Write([]byte(csrfDerivationLabel))
	return mac.Sum(nil)
}

// csrfHMAC computes the MAC half of the CSRF token: HMAC over
// the session cookie value and the random, under this use's own key.
// Binding the MAC to the session id makes leaked tokens useless across
// sessions.
func csrfHMAC(masterKey []byte, sessionCookieValue, random string) string {
	mac := hmac.New(sha256.New, csrfKey(masterKey))
	mac.Write([]byte(sessionCookieValue))
	mac.Write([]byte(".")) // separator: base64url parts contain no '.'
	mac.Write([]byte(random))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// csrfConstantEqual compares the expected and provided MAC halves in
// constant time. Both are base64url; malformed inputs simply fail.
func csrfConstantEqual(wantB64, gotB64 string) bool {
	want, err := base64.RawURLEncoding.DecodeString(wantB64)
	if err != nil {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(gotB64)
	if err != nil {
		return false
	}
	return hmac.Equal(want, got)
}

// hashSessionID is the at-rest transformation: only the SHA-256
// of the cookie value is ever persisted — the same function on both the
// insert path (login) and the lookup path (RequireAdmin/logout).
func hashSessionID(cookieValue string) string {
	sum := sha256.Sum256([]byte(cookieValue))
	return hex.EncodeToString(sum[:])
}

// sessionCookieValue reads the session cookie under either name:
// __Host-id when the login arrived over TLS, plain id otherwise.
func sessionCookieValue(r *http.Request) string {
	for _, name := range []string{"__Host-" + sessionCookieName, sessionCookieName} {
		if c, err := r.Cookie(name); err == nil && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

// rawIDEncoded renders the raw session id bytes as the cookie value.
func rawIDEncoded(raw []byte) string {
	return base64.RawURLEncoding.EncodeToString(raw)
}

func isMutation(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// requestOrigin reconstructs this request's own origin (scheme://host) for
// the exact-match comparison. Behind a reverse proxy the connection state
// is the proxy's, not the browser's: a TLS terminator leaves r.TLS nil and
// the Host may be the upstream address. When the immediate peer is a
// configured trusted proxy, its X-Forwarded-Proto/X-Forwarded-Host carry
// the public origin instead — and only then: an untrusted client must not
// spoof its way past the check by setting those headers itself.
func (sm *SessionManager) requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if sm.trustedForwarder(r) {
		// Only real schemes: garbage falls back to the connection state
		// (and the comparison then fails, never passes).
		if p := firstForwarded(r.Header.Get("X-Forwarded-Proto")); p == "http" || p == "https" {
			scheme = p
		}
		if h := firstForwarded(r.Header.Get("X-Forwarded-Host")); h != "" {
			host = h
		}
	}
	return scheme + "://" + host
}

// trustedForwarder reports whether the request's immediate peer is a
// configured trusted proxy. An empty trust set trusts no one.
func (sm *SessionManager) trustedForwarder(r *http.Request) bool {
	if len(sm.trustedProxies) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range sm.trustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// firstForwarded takes the leftmost comma-separated element of a forwarded
// header (the client-facing value when proxies append) and normalizes case:
// the comparison against the browser's Origin is an exact string match.
func firstForwarded(v string) string {
	first, _, _ := strings.Cut(v, ",")
	return strings.ToLower(strings.TrimSpace(first))
}

// ParseTrustedProxies validates the operator's trusted-proxy entries into
// the match set WithTrustedProxies consumes. A CIDR is taken as given; a
// bare IP becomes its own /32 or /128 (exactly itself, never a wider
// match). Invalid entries error — a typo must fail startup loudly, never
// silently widen or narrow the trust boundary.
func ParseTrustedProxies(entries []string) ([]*net.IPNet, error) {
	var nets []*net.IPNet
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !strings.Contains(e, "/") {
			ip := net.ParseIP(e)
			if ip == nil {
				return nil, fmt.Errorf("auth: trusted proxy %q: not an IP or CIDR", e)
			}
			bits := 8 * net.IPv6len
			if ip.To4() != nil {
				bits = 8 * net.IPv4len
			}
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, ipnet, err := net.ParseCIDR(e)
		if err != nil {
			return nil, fmt.Errorf("auth: trusted proxy %q: %w", e, err)
		}
		nets = append(nets, ipnet)
	}
	return nets, nil
}

func setSessionCookie(w http.ResponseWriter, value string, tlsArrival bool) {
	name := sessionCookieName
	secure := false
	if tlsArrival {
		// __Host- requires Secure + Path=/ + no Domain.
		name = "__Host-" + sessionCookieName
		secure = true
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is set iff the request arrived over TLS (per-request split) — on plain HTTP a Secure cookie would never be sent back
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		// No Expires/Max-Age: non-persistent.
	})
}

func setCSRFCookie(w http.ResponseWriter, value string, tlsArrival bool) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: deliberately not HttpOnly; Secure follows the TLS split like the session cookie
		Name:     csrfCookieName,
		Value:    value,
		Path:     "/",
		Secure:   tlsArrival,
		SameSite: http.SameSiteStrictMode,
		// Deliberately NOT HttpOnly: the SPA must be able to read the
		// random half and echo the full token via X-CSRF-Token.
	})
}

func clearSessionCookie(w http.ResponseWriter, tlsArrival bool) {
	name := sessionCookieName
	if tlsArrival {
		name = "__Host-" + sessionCookieName
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: deletion cookie — MaxAge<0 expires it; Secure is irrelevant for a valueless clear
		Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

func clearCSRFCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: deletion cookie — MaxAge<0 expires it
		Name: csrfCookieName, Value: "", Path: "/", MaxAge: -1,
		SameSite: http.SameSiteStrictMode,
	})
}

// writeAdminUnauthorized is the uniform 401: identical body
// whether the failure is a missing, unknown, idle-expired, or
// absolutely-expired session — or bad login credentials.
func writeAdminUnauthorized(w http.ResponseWriter) {
	writeAdminError(w, http.StatusUnauthorized, "unauthorized")
}

// writeAdminError emits the admin-surface error shape. It never echoes
// upstream material and never varies with the failure cause.
func writeAdminError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
