package auth

// WebUI session auth + CSRF middleware, implementing the committed Phase 2
// research contract (docs/research/phase-2-best-practices.md §10):
//   - S-1: 256-bit CSPRNG session IDs, SHA-256-hashed at rest
//   - S-2/S-3: generic cookie name `id`, `__Host-id` when the request
//     arrived over TLS (per-request split)
//   - S-4/S-5: HttpOnly, non-persistent, SameSite=Strict
//   - S-6: strict mode — unknown IDs are rejected, never adopted
//   - S-7: all session state server-side; the raw ID lives only in the cookie
//   - S-8: a fresh session per login (renewal)
//   - S-9: 30 min idle / 12 h absolute, enforced against DB timestamps
//   - S-10: logout = row delete + cookie clear
//   - C-1/C-2: CSRF token = random + HMAC-SHA-256(master key, sessionID‖random)
//   - C-3: X-CSRF-Token + Content-Type application/json required on mutations
//   - C-4: simple content types on mutations → 415
//   - C-6: Origin (when present) must match scheme+host exactly → 403
//
// Failure responses never distinguish causes (Analysis G: 401 bodies are
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
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/proxyllm/proxyllm/internal/storage"
)

const (
	// sessionCookieName is deliberately generic (S-2): it reveals nothing
	// about the software in front of the proxy.
	sessionCookieName = "id"
	// csrfCookieName carries only the random half of the CSRF token (C-1);
	// the full token is returned to the SPA at login and echoed via the
	// X-CSRF-Token header. JS-readable — NOT HttpOnly.
	csrfCookieName = "csrf"

	// S-9 windows, in seconds, enforced server-side against store timestamps.
	sessionIdleWindow  = 30 * 60
	sessionAbsoluteCap = 12 * 60 * 60

	// S-1: 256-bit session IDs. C-1: 128-bit CSRF randomness is plenty —
	// the MAC carries the security, the random only binds cookie↔header.
	sessionIDBytes  = 32
	csrfRandomBytes = 16
)

// dummyArgon2PHC is a well-formed Argon2id PHC string at OWASP-minimum
// parameters whose digest matches nothing. Unknown usernames run a verify
// against it so login timing cannot distinguish "no such user" from "wrong
// password" (Analysis G: uniform 401 shape — body AND observable behavior).
const dummyArgon2PHC = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA"

// SessionManager owns the WebUI login/logout handlers and the admin
// middleware. now is injectable so the idle/absolute windows are testable
// against pinned clocks.
type SessionManager struct {
	st        *storage.Store
	masterKey []byte
	now       func() int64
}

// NewSessionManager wires a manager over st with the CSRF master key
// (32 bytes recommended; used only as an HMAC secret, C-1/C-2) and a clock.
func NewSessionManager(st *storage.Store, masterKey []byte, now func() int64) *SessionManager {
	if now == nil {
		now = func() int64 { return time.Now().Unix() }
	}
	return &SessionManager{st: st, masterKey: masterKey, now: now}
}

type adminCtxKey struct{}

// AdminIDFromContext returns the authenticated admin id for requests that
// passed RequireAdmin ("" otherwise). Handlers use it for audit entries —
// never for authorization decisions of their own.
func AdminIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(adminCtxKey{}).(string)
	return id
}

type rawSessionCtxKey struct{}

// rawSessionIDFromContext exposes the raw (unhashed) session id to the
// logout handler; it never leaves the auth package.
func rawSessionIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(rawSessionCtxKey{}).(string)
	return id
}

// HandleLogin authenticates an admin (Argon2id) and issues a brand-new
// session (S-8) plus the CSRF pair. Failures are uniform 401s.
func (sm *SessionManager) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAdminError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10) // Analysis G: bounded input
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
		hasher().Verify(dummyArgon2PHC, creds.Password)
		writeAdminUnauthorized(w)
		return
	}
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !hasher().Verify(admin.PasswordHash, creds.Password) {
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
// the CSRF checks apply), then deletes the session row (S-10) and clears
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

// RequireAdmin wraps admin handlers with session auth (S-6/S-9: strict,
// server-side windows) and, for state-changing methods, the CSRF layers
// (C-3/C-4/C-6): application/json only, Origin match when present, valid
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
		// S-9: both windows checked server-side against store timestamps —
		// a doctored client clock is irrelevant.
		if now-sess.LastSeenAt > sessionIdleWindow || now >= sess.AbsoluteExpiresAt {
			writeAdminUnauthorized(w)
			return
		}
		if err := sm.st.TouchSession(r.Context(), idHash, now); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// Token re-issue (C-1/C-2): the SPA holds the CSRF token only in
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
			// C-4: reject simple content types — a cross-site form or
			// text/plain fetch must never be processable without preflight.
			mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mt != "application/json" {
				writeAdminError(w, http.StatusUnsupportedMediaType, "content type must be application/json")
				return
			}
			// C-6: when Origin is present it must match scheme+host exactly.
			if origin := r.Header.Get("Origin"); origin != "" && origin != requestOrigin(r) {
				writeAdminError(w, http.StatusForbidden, "origin mismatch")
				return
			}
			// C-5: the token is checked regardless of SameSite protection.
			header := r.Header.Get("X-CSRF-Token")
			random, mac, ok := strings.Cut(header, ".")
			if !ok || !csrfConstantEqual(
				csrfHMAC(sm.masterKey, raw, random), mac) {
				writeAdminError(w, http.StatusForbidden, "csrf check failed")
				return
			}
		}

		ctx := context.WithValue(r.Context(), adminCtxKey{}, sess.AdminID)
		ctx = context.WithValue(ctx, rawSessionCtxKey{}, raw)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// csrfRandomFor returns the CSRF random half for this session: the csrf
// cookie's value when present (the common case — token stability across
// requests), otherwise a freshly minted one, set as a replacement cookie.
// The random alone authorizes nothing (C-2: the MAC carries the security);
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

// csrfHMAC computes the MAC half of the CSRF token (C-1/C-2): HMAC over
// the session cookie value and the random, keyed by the master key.
// Binding the MAC to the session id makes leaked tokens useless across
// sessions.
func csrfHMAC(masterKey []byte, sessionCookieValue, random string) string {
	mac := hmac.New(sha256.New, masterKey)
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

// hashSessionID is the at-rest transformation (S-1/S-7): only the SHA-256
// of the cookie value is ever persisted — the same function on both the
// insert path (login) and the lookup path (RequireAdmin/logout).
func hashSessionID(cookieValue string) string {
	sum := sha256.Sum256([]byte(cookieValue))
	return hex.EncodeToString(sum[:])
}

// sessionCookieValue reads the session cookie under either name (S-3):
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
// the C-6 exact-match comparison.
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func setSessionCookie(w http.ResponseWriter, value string, tlsArrival bool) {
	name := sessionCookieName
	secure := false
	if tlsArrival {
		// __Host- requires Secure + Path=/ + no Domain (S-3).
		name = "__Host-" + sessionCookieName
		secure = true
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is set iff the request arrived over TLS (S-3 per-request split) — on plain HTTP a Secure cookie would never be sent back
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true, // S-4
		Secure:   secure,
		SameSite: http.SameSiteStrictMode, // S-5
		// No Expires/Max-Age: non-persistent (S-4).
	})
}

func setCSRFCookie(w http.ResponseWriter, value string, tlsArrival bool) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: deliberately not HttpOnly (C-1); Secure follows the S-3 TLS split like the session cookie
		Name:     csrfCookieName,
		Value:    value,
		Path:     "/",
		Secure:   tlsArrival,
		SameSite: http.SameSiteStrictMode,
		// Deliberately NOT HttpOnly (C-1): the SPA must be able to read the
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

// writeAdminUnauthorized is the uniform 401 (Analysis G): identical body
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
