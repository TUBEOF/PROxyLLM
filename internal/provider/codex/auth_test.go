package codex_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/proxyllm/proxyllm/internal/provider/codex"
	"github.com/proxyllm/proxyllm/internal/storage"
)

// Codex OAuth login contract tests, RED-GREEN (plan §6.3, Analysis F).
// Expectations derive from the live-researched codex-rs facts (digest §5b,
// accessed 2026-09-11/12) — never from provider code behavior:
//   - X17: authorize URL params, ports 1455/1457, redirect_uri shape,
//     127.0.0.1-only listener, explicit code_challenge_method=S256.
//   - X21: verifier = 64 random bytes, base64url NO padding (86 chars);
//     challenge = base64url-nopad(SHA-256(verifier string)).
//   - X16: token exchange is form-encoded grant_type=authorization_code.
//   - X9:  refresh grant is JSON, Content-Type application/json.
//   - X11: refresh tokens are single-use; refresh_token_reused → operator
//     "re-login required" error, never a silent loop.
//   - X12: rotation persists atomically; refresh is single-flight.
//   - X13: client ID default app_EMoamEEZ73f0CkXaXp7hrann + override.
//   - X14: originator codex_cli_rs rides authorize URL + token requests.
//   - X20: device flow wire shape, string interval, server-provided PKCE,
//     403/404 keep-polling, other errors fatal, 15-min cap, 404-on-usercode
//     = disabled → operator error.

// freshStore returns an empty in-memory token store (helpers in
// codex_test.go).
func freshStore() *memStore { return newMemStore() }

func testAuth(t *testing.T, issuer string, store codex.TokenStore) codex.Auth {
	t.Helper()
	return codex.NewAuthClient(codex.AuthConfig{
		Issuer:     issuer,
		ClientID:   codex.OAuthClientID, // the pinned default; override below
		HTTP:       http.DefaultClient,
		Store:      store,
		ProviderID: providerID, // align with the seeded token rows
	})
}

// --- PKCE (X21) ---------------------------------------------------------------

func TestPKCE_VerifierShape(t *testing.T) {
	for i := 0; i < 32; i++ {
		verifier, err := codex.NewVerifier()
		if err != nil {
			t.Fatalf("NewVerifier: %v", err)
		}
		if len(verifier) != 86 {
			t.Fatalf("verifier length = %d, want 86 (64 bytes, base64url unpadded, X21)", len(verifier))
		}
		if strings.ContainsAny(verifier, "+/=") {
			t.Fatalf("verifier %q contains non-URL-safe or padding chars (X21)", verifier)
		}
		raw, err := base64.RawURLEncoding.DecodeString(verifier)
		if err != nil {
			t.Fatalf("verifier is not base64url-nopad: %v", err)
		}
		if len(raw) != 64 {
			t.Fatalf("decoded verifier = %d bytes, want 64 (X21)", len(raw))
		}
	}
}

func TestPKCE_VerifierUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		v, err := codex.NewVerifier()
		if err != nil {
			t.Fatal(err)
		}
		if seen[v] {
			t.Fatal("verifier repeated — must be cryptographically random (X21)")
		}
		seen[v] = true
	}
}

func TestPKCE_Challenge(t *testing.T) {
	// Deterministic vector: challenge = base64url-nopad(SHA-256(verifier)).
	// 86 chars — the canonical base64url-nopad length of a 64-byte verifier (X21).
	verifier := "AAcOFRwjKjE4P0ZNVFtiaXB3foWMk5qhqK-2vcTL0tng5-71_AMKERgfJi00O0JJUFdeZWxzeoGIj5adpKuyuQ"
	if len(verifier) != 86 {
		t.Fatalf("fixture verifier length = %d, want 86", len(verifier))
	}
	got := codex.Challenge(verifier)
	if strings.ContainsAny(got, "+/=") {
		t.Fatalf("challenge %q contains non-URL-safe or padding chars (X21)", got)
	}
	// The challenge must equal the unpadded base64url of the SHA-256 digest
	// of the verifier STRING bytes.
	want := sha256B64URL([]byte(verifier))
	if got != want {
		t.Errorf("Challenge = %q, want %q (S256 over verifier string bytes, X21)", got, want)
	}
}

// --- authorize URL (X17) -------------------------------------------------------

func TestBuildAuthorizeURL(t *testing.T) {
	store := freshStore()
	auth := testAuth(t, "https://auth.openai.com", store)
	raw, verifier, err := auth.BuildAuthorizeURL("state-1", 1455)
	if err != nil {
		t.Fatalf("BuildAuthorizeURL: %v", err)
	}
	if verifier == "" {
		t.Fatal("BuildAuthorizeURL must return the verifier for the exchange")
	}

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("authorize URL unparseable: %v", err)
	}
	if u.Scheme != "https" || u.Host != "auth.openai.com" {
		t.Errorf("authorize host = %s, want auth.openai.com (X1)", u.Host)
	}
	if u.Path != "/oauth/authorize" {
		t.Errorf("authorize path = %s, want /oauth/authorize", u.Path)
	}
	q := u.Query()
	for key, want := range map[string]string{
		"response_type":              "code",
		"client_id":                  "app_EMoamEEZ73f0CkXaXp7hrann", // X13 default
		"redirect_uri":               "http://localhost:1455/auth/callback",
		"scope":                      "openid profile email offline_access api.connectors.read api.connectors.invoke",
		"code_challenge_method":      "S256", // X17: explicitly its own param
		"state":                      "state-1",
		"originator":                 "codex_cli_rs", // X14
		"id_token_add_organizations": "true",
		"codex_cli_simplified_flow":  "true",
	} {
		if got := q.Get(key); got != want {
			t.Errorf("param %s = %q, want %q", key, got, want)
		}
	}
	if ch := q.Get("code_challenge"); ch == "" {
		t.Error("code_challenge missing from authorize URL (X17)")
	} else if ch != codex.Challenge(verifier) {
		t.Errorf("code_challenge %q does not match the returned verifier (X21)", ch)
	}
}

func TestBuildAuthorizeURL_ClientIDOverride(t *testing.T) {
	store := freshStore()
	auth := codex.NewAuthClient(codex.AuthConfig{
		Issuer:     "https://auth.openai.com",
		ClientID:   "app_custom_override", // X13: operator override wins
		HTTP:       http.DefaultClient,
		Store:      store,
		ProviderID: providerID,
	})
	raw, _, err := auth.BuildAuthorizeURL("s", 1455)
	if err != nil {
		t.Fatal(err)
	}
	q, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := q.Query().Get("client_id"); got != "app_custom_override" {
		t.Errorf("client_id = %q, want the configured override (X13)", got)
	}
}

func TestBuildAuthorizeURL_PortFallback(t *testing.T) {
	store := freshStore()
	auth := testAuth(t, "https://auth.openai.com", store)

	// Port 1455 is the default; the caller may pass the 1457 fallback (X3).
	raw, _, err := auth.BuildAuthorizeURL("s", 1457)
	if err != nil {
		t.Fatal(err)
	}
	q, _ := url.Parse(raw)
	if got := q.Query().Get("redirect_uri"); got != "http://localhost:1457/auth/callback" {
		t.Errorf("redirect_uri = %q, want the 1457 fallback (X3)", got)
	}
}

// --- interactive login: callback listener (X1/X3/X17) -------------------------

func TestLoginFromBrowser_ExchangeAndPersist(t *testing.T) {
	var exchange atomic.Value // captured form body
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Errorf("unexpected issuer path %s", r.URL.Path)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("exchange body must be form-encoded (X16): %v", err)
		}
		if got := r.Form.Get("grant_type"); got != "authorization_code" {
			t.Errorf("exchange grant_type = %q, want authorization_code (X16)", got)
		}
		if got := r.Form.Get("client_id"); got != "app_EMoamEEZ73f0CkXaXp7hrann" {
			t.Errorf("exchange client_id = %q, want pinned default (X13)", got)
		}
		if got := r.Form.Get("code_verifier"); got == "" {
			t.Error("exchange code_verifier missing (X16)")
		}
		exchange.Store(r.Form)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":"a1","refresh_token":"r1","id_token":%q}`+"\n", makeJWT(t, "acct-1"))
	}))
	t.Cleanup(srv.Close)

	store := freshStore()
	auth := testAuth(t, srv.URL, store)

	// Simulate the browser hitting the callback with code + state.
	res, err := auth.LoginFromBrowser(context.Background(), codex.BrowserLogin{
		BindAddr: "127.0.0.1:0", // ephemeral test port; production uses 1455/1457
		OpenURL:  func(authorizeURL string) error { return simulateBrowser(t, authorizeURL, "authcode-1") },
		Timeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("LoginFromBrowser: %v", err)
	}

	form, _ := exchange.Load().(url.Values)
	if form == nil {
		t.Fatal("exchange never reached the issuer")
	}
	if got := form.Get("code"); got != "authcode-1" {
		t.Errorf("exchange code = %q, want authcode-1 from the callback", got)
	}
	// redirect_uri must exactly match the one registered in the authorize
	// URL — derive it from the authorize URL the login produced.
	if got := form.Get("redirect_uri"); got == "" {
		t.Error("exchange redirect_uri missing (X16)")
	}
	if res.AccountID != "acct-1" {
		t.Errorf("AccountID = %q, want acct-1 from the id_token claim (X15)", res.AccountID)
	}
	if got := store.get(codex.KindAccessToken); got != "a1" {
		t.Errorf("access_token = %q, want a1 persisted", got)
	}
	if got := store.get(codex.KindRefreshToken); got != "r1" {
		t.Errorf("refresh_token = %q, want r1 persisted", got)
	}
	if got := store.get(codex.KindIDToken); got == "" {
		t.Error("id_token not persisted")
	}
}

// TestLoginFromBrowser_StateRejected: a callback carrying a different state
// is refused — the listener answers, but no exchange happens (OAuth security
// baseline; RFC 9700 OA-1 adjacent).
func TestLoginFromBrowser_StateRejected(t *testing.T) {
	var exchanged atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanged.Store(true)
	}))
	t.Cleanup(srv.Close)

	store := freshStore()
	auth := testAuth(t, srv.URL, store)
	_, err := auth.LoginFromBrowser(context.Background(), codex.BrowserLogin{
		BindAddr: "127.0.0.1:0",
		OpenURL:  func(authorizeURL string) error { return simulateBrowser(t, authorizeURL, "code", "wrong-state") },
		Timeout:  5 * time.Second,
	})
	if err == nil {
		t.Fatal("state mismatch must fail the login")
	}
	if exchanged.Load() {
		t.Error("state mismatch must never reach the token exchange")
	}
}

// TestLoginFromBrowser_BindLocalhostOnly: the callback listener must bind
// 127.0.0.1 only — never 0.0.0.0 (Analysis G control 7).
func TestLoginFromBrowser_BindLocalhostOnly(t *testing.T) {
	store := freshStore()
	auth := testAuth(t, "https://auth.openai.com", store)
	_, err := auth.LoginFromBrowser(context.Background(), codex.BrowserLogin{
		BindAddr: "0.0.0.0:0", // hostile bind request must be refused
		OpenURL:  func(string) error { return nil },
		Timeout:  100 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("binding 0.0.0.0 must be refused (Analysis G control 7)")
	}
}

// --- refresh grant (X9/X11/X12) ----------------------------------------------

func TestRefresh_JSONGrant(t *testing.T) {
	var sawRefreshToken atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("refresh Content-Type = %q, want application/json (X9)", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("refresh body must be JSON (X9): %v", err)
		}
		if body["grant_type"] != "refresh_token" {
			t.Errorf("grant_type = %v, want refresh_token (X9)", body["grant_type"])
		}
		if body["client_id"] != "app_EMoamEEZ73f0CkXaXp7hrann" {
			t.Errorf("client_id = %v, want pinned default (X13)", body["client_id"])
		}
		sawRefreshToken.Store(body["refresh_token"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"a2","refresh_token":"r2"}`)
	}))
	t.Cleanup(srv.Close)

	store := freshStore()
	auth := testAuth(t, srv.URL, store)
	if err := seedRefreshable(store, "r1"); err != nil {
		t.Fatal(err)
	}

	if err := auth.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got, _ := sawRefreshToken.Load().(string); got != "r1" {
		t.Errorf("refresh_token sent = %q, want the stored r1 (X9)", got)
	}
	if got := store.get(codex.KindAccessToken); got != "a2" {
		t.Errorf("access_token = %q, want a2 (rotation persisted)", got)
	}
	if got := store.get(codex.KindRefreshToken); got != "r2" {
		t.Errorf("refresh_token = %q, want r2 (single-use rotation persisted, X11)", got)
	}
}

func TestRefresh_ReusedTokenIsOperatorError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"refresh_token_reused","error_description":"refresh token has already been used"}`)
	}))
	t.Cleanup(srv.Close)

	store := freshStore()
	auth := testAuth(t, srv.URL, store)
	if err := seedRefreshable(store, "r1"); err != nil {
		t.Fatal(err)
	}

	err := auth.Refresh(context.Background())
	if err == nil {
		t.Fatal("refresh_token_reused must fail (X11)")
	}
	var re *codex.ReLoginRequiredError
	if !errors.As(err, &re) {
		t.Fatalf("error = %v (%T), want *codex.ReLoginRequiredError (X11/X12 operator action)", err, err)
	}
	// The consumed token must NOT be rotated in place on failure.
	if got := store.get(codex.KindRefreshToken); got != "r1" {
		t.Errorf("refresh_token = %q, want the pre-failure r1 untouched", got)
	}
}

func TestRefresh_SingleFlight(t *testing.T) {
	var hits atomic.Int32
	arrived := make(chan struct{}, 4) // late callers park here
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if hits.Load() == 1 {
			<-release // the leader's rotation holds until the followers have parked
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"a2","refresh_token":"r2"}`)
	}))
	t.Cleanup(srv.Close)

	store := freshStore()
	auth := testAuth(t, srv.URL, store)
	if err := seedRefreshable(store, "r1"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			arrived <- struct{}{} // parked: inside Refresh, before any result
			errs[i] = auth.Refresh(context.Background())
		}(i)
	}
	// Force overlap: wait until all four goroutines have STARTED (parked on
	// the flight), only then let the leader's round trip finish.
	for i := 0; i < 4; i++ {
		<-arrived
	}
	close(release)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent refresh %d: %v", i, err)
		}
	}
	// X12 single-flight: concurrent refreshes coalesce around one rotation;
	// every caller — leader and late arrivals alike — ends with the LEADER's
	// rotated pair rather than racing a second refresh with the consumed r1.
	if hits.Load() != 1 {
		t.Errorf("refresh hits = %d, want exactly 1 for 4 concurrent calls (single-flight, X12)", hits.Load())
	}
	if got := store.get(codex.KindAccessToken); got != "a2" {
		t.Errorf("access_token = %q, want the leader's a2 shared by all callers", got)
	}
	if got := store.get(codex.KindRefreshToken); got != "r2" {
		t.Errorf("refresh_token = %q, want the leader's r2 shared by all callers", got)
	}
}

// TestRefresh_FlightWait_ParksWaitersOnFlight: X12 white-box complement to the
// forced-overlap test above. While the leader is provably INSIDE its issuer
// round trip, a second caller must park on the flight (not the lastRefresh
// short-circuit — lastRefresh is still zero) and must return the LEADER's
// error, not trigger a second rotation.
func TestRefresh_FlightWait_ParksWaitersOnFlight(t *testing.T) {
	leaderInHandler := make(chan struct{})
	release := make(chan struct{})
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			close(leaderInHandler)
			<-release // leader stays inside the round trip until the waiter parked
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"a2","refresh_token":"r2"}`)
	}))
	t.Cleanup(srv.Close)

	store := freshStore()
	auth := testAuth(t, srv.URL, store)
	if err := seedRefreshable(store, "r1"); err != nil {
		t.Fatal(err)
	}

	leaderDone := make(chan error, 1)
	go func() { leaderDone <- auth.Refresh(context.Background()) }()
	// Wait until the leader is provably inside the HTTP handler — the flight
	// is set and lastRefresh is still zero at this point.
	<-leaderInHandler

	// Second caller: lastRefresh is zero, so the short-circuit cannot fire;
	// a.flight is set, so Refresh MUST park on <-f.done and adopt f.err.
	waiterDone := make(chan error, 1)
	go func() { waiterDone <- auth.Refresh(context.Background()) }()

	// Give the waiter a scheduling head start; it can only be inside the
	// flight-wait branch (server hits prove no second rotation started).
	select {
	case <-waiterDone:
		t.Fatal("waiter returned before the leader's rotation completed — it did not park on the flight")
	case <-time.After(50 * time.Millisecond):
	}
	if hits.Load() != 1 {
		t.Fatalf("refresh hits = %d, want 1 — a second rotation started while the leader was in flight", hits.Load())
	}

	close(release)
	if err := <-leaderDone; err != nil {
		t.Errorf("leader refresh: %v", err)
	}
	if err := <-waiterDone; err != nil {
		t.Errorf("waiter refresh: %v (want the flight's shared result)", err)
	}
	if hits.Load() != 1 {
		t.Errorf("refresh hits = %d, want exactly 1 (single-flight, X12)", hits.Load())
	}
	if got := store.get(codex.KindAccessToken); got != "a2" {
		t.Errorf("access_token = %q, want the leader's a2", got)
	}
}

// --- device flow (X20) --------------------------------------------------------

func TestDeviceFlow_ExchangeAndPersist(t *testing.T) {
	var usercodeBody, pollBody map[string]any
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			if err := json.NewDecoder(r.Body).Decode(&usercodeBody); err != nil {
				t.Errorf("usercode body must be JSON: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			// X20: interval arrives as a STRING; user_code key.
			_, _ = io.WriteString(w, `{"device_auth_id":"dev-1","user_code":"WDJB-MJHT","interval":"5"}`)
		case "/api/accounts/deviceauth/token":
			if err := json.NewDecoder(r.Body).Decode(&pollBody); err != nil {
				t.Errorf("poll body must be JSON: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			// X20: the SERVER provides the PKCE pair; redirect_uri is the
			// issuer deviceauth callback.
			_, _ = io.WriteString(w, `{"authorization_code":"ac-1","code_challenge":"chal","code_verifier":"ver","redirect_uri":"/deviceauth/callback"}`)
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Errorf("exchange must be form-encoded (X16): %v", err)
			}
			if got := r.Form.Get("redirect_uri"); got != srv.URL+"/deviceauth/callback" {
				t.Errorf("device exchange redirect_uri = %q, want {issuer}/deviceauth/callback (X20)", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"access_token":"a1","refresh_token":"r1","id_token":%q}`+"\n", makeJWT(t, "acct-9"))
		default:
			t.Errorf("unexpected issuer path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	store := freshStore()
	auth := testAuth(t, srv.URL, store)
	res, err := auth.LoginFromDevice(context.Background(), codex.DeviceLogin{
		OpenURL: func(u string) error { return nil },
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("LoginFromDevice: %v", err)
	}

	if usercodeBody["client_id"] != "app_EMoamEEZ73f0CkXaXp7hrann" {
		t.Errorf("usercode client_id = %v, want pinned default (X13)", usercodeBody["client_id"])
	}
	if pollBody["device_auth_id"] != "dev-1" || pollBody["user_code"] != "WDJB-MJHT" {
		t.Errorf("poll body = %v, want the device_auth_id + user_code pair (X20)", pollBody)
	}
	if res.AccountID != "acct-9" {
		t.Errorf("AccountID = %q, want acct-9", res.AccountID)
	}
	if got := store.get(codex.KindAccessToken); got != "a1" {
		t.Errorf("access_token = %q, want a1 persisted", got)
	}
	if got := store.get(codex.KindRefreshToken); got != "r1" {
		t.Errorf("refresh_token = %q, want r1 persisted", got)
	}
}

func TestDeviceFlow_PendingThenSuccess(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"device_auth_id":"dev-1","user_code":"WDJB","interval":"1"}`)
		case "/api/accounts/deviceauth/token":
			// X20: 403/404 while the user has not yet approved — keep polling.
			if polls.Add(1) < 3 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"authorization_code":"ac","code_challenge":"c","code_verifier":"v"}`)
		case "/oauth/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"a1","refresh_token":"r1"}`)
		}
	}))
	t.Cleanup(srv.Close)

	store := freshStore()
	auth := testAuth(t, srv.URL, store)
	_, err := auth.LoginFromDevice(context.Background(), codex.DeviceLogin{
		OpenURL: func(u string) error { return nil },
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("pending→success device login must succeed: %v", err)
	}
	if polls.Load() < 3 {
		t.Errorf("polls = %d, want ≥ 3 (403 must not abort, X20)", polls.Load())
	}
}

func TestDeviceFlow_OtherStatusIsFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"device_auth_id":"dev-1","user_code":"WDJB","interval":"1"}`)
		case "/api/accounts/deviceauth/token":
			w.WriteHeader(http.StatusInternalServerError) // X20: not 403/404 → fatal
		}
	}))
	t.Cleanup(srv.Close)

	store := freshStore()
	auth := testAuth(t, srv.URL, store)
	_, err := auth.LoginFromDevice(context.Background(), codex.DeviceLogin{
		OpenURL: func(u string) error { return nil },
		Timeout: 5 * time.Second,
	})
	if err == nil {
		t.Fatal("500 on the poll must be fatal (X20)")
	}
}

func TestDeviceFlow_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"device_auth_id":"dev-1","user_code":"WDJB","interval":"1"}`)
		case "/api/accounts/deviceauth/token":
			w.WriteHeader(http.StatusNotFound) // X20: keep polling… past the deadline
		}
	}))
	t.Cleanup(srv.Close)

	store := freshStore()
	auth := testAuth(t, srv.URL, store)
	start := time.Now()
	_, err := auth.LoginFromDevice(context.Background(), codex.DeviceLogin{
		OpenURL: func(u string) error { return nil },
		Timeout: 2 * time.Second, // X20: hard cap (CLI: 15 min)
	})
	if err == nil {
		t.Fatal("device login must fail at the deadline (X20)")
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Errorf("deadline overshot: %v", elapsed)
	}
}

func TestDeviceFlow_DisabledIsOperatorError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// X20: 404 on the INITIAL usercode request = device login disabled.
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	store := freshStore()
	auth := testAuth(t, srv.URL, store)
	_, err := auth.LoginFromDevice(context.Background(), codex.DeviceLogin{
		OpenURL: func(u string) error { return nil },
		Timeout: 5 * time.Second,
	})
	var op *codex.DeviceLoginDisabledError
	if !errors.As(err, &op) {
		t.Fatalf("error = %v (%T), want *codex.DeviceLoginDisabledError (X20)", err, err)
	}
}

// --- stepwise device flow (X20, admin-API form) --------------------------------
//
// The admin API drives the device flow stepwise: StartDeviceLogin surfaces the
// user_code and verification URL to the operator (the blocking LoginFromDevice
// keeps them internal — the operator must enter the code on the verification
// page, so the UI has to show it), and Poll performs exactly ONE issuer round
// trip per call so the SPA polls at the issuer-provided interval. Expectations
// derive from X20 (usercode wire shape, string interval, 403/404 pending, other
// errors fatal, server-provided PKCE, {issuer}/deviceauth/callback exchange,
// 15-min cap, 404-on-usercode = disabled) and the delegated X16 exchange —
// never from provider code behavior.

func TestStepwiseDevice_StartSurfacesUserCode(t *testing.T) {
	var usercodeHits, tokenHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			usercodeHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			// X20: interval arrives as a STRING; user_code key.
			_, _ = io.WriteString(w, `{"device_auth_id":"dev-7","user_code":"WDJB-MJHT","interval":"5"}`)
		case "/api/accounts/deviceauth/token":
			tokenHits.Add(1)
			w.WriteHeader(http.StatusForbidden)
		default:
			t.Errorf("unexpected issuer path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	store := freshStore()
	auth := testAuth(t, srv.URL, store)
	sess, err := auth.StartDeviceLogin(context.Background(), 15*time.Minute)
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	if sess.DeviceAuthID != "dev-7" || sess.UserCode != "WDJB-MJHT" {
		t.Errorf("session = %q/%q, want dev-7/WDJB-MJHT (X20)", sess.DeviceAuthID, sess.UserCode)
	}
	if sess.VerificationURL != srv.URL+"/codex/device" {
		t.Errorf("VerificationURL = %q, want {issuer}/codex/device (X20)", sess.VerificationURL)
	}
	if sess.Interval != 5*time.Second {
		t.Errorf("Interval = %v, want 5s parsed from the STRING field (X20)", sess.Interval)
	}
	if tokenHits.Load() != 0 {
		t.Errorf("token polls during start = %d, want 0 — start only requests the usercode", tokenHits.Load())
	}
	if usercodeHits.Load() != 1 {
		t.Errorf("usercode requests = %d, want 1", usercodeHits.Load())
	}
}

func TestStepwiseDevice_PollPendingCarriesWirePair(t *testing.T) {
	var pollBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"device_auth_id":"dev-1","user_code":"WDJB","interval":"5"}`)
		case "/api/accounts/deviceauth/token":
			if err := json.NewDecoder(r.Body).Decode(&pollBody); err != nil {
				t.Errorf("poll body must be JSON: %v", err)
			}
			w.WriteHeader(http.StatusForbidden) // X20: 403 = pending
		default:
			t.Errorf("unexpected issuer path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	store := freshStore()
	auth := testAuth(t, srv.URL, store)
	sess, err := auth.StartDeviceLogin(context.Background(), time.Minute)
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	approved, _, err := sess.Poll(context.Background())
	if err != nil {
		t.Fatalf("pending poll must not error: %v", err)
	}
	if approved {
		t.Fatal("approved = true on 403, want pending (X20)")
	}
	if pollBody["device_auth_id"] != "dev-1" || pollBody["user_code"] != "WDJB" {
		t.Errorf("poll body = %v, want the device_auth_id + user_code pair (X20)", pollBody)
	}
}

func TestStepwiseDevice_PollThrottlesToIssuerInterval(t *testing.T) {
	var tokenHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"device_auth_id":"dev-1","user_code":"WDJB","interval":"5"}`)
		case "/api/accounts/deviceauth/token":
			tokenHits.Add(1)
			w.WriteHeader(http.StatusForbidden)
		default:
			t.Errorf("unexpected issuer path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	store := freshStore()
	auth := testAuth(t, srv.URL, store)
	sess, err := auth.StartDeviceLogin(context.Background(), time.Minute)
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	if approved, _, err := sess.Poll(context.Background()); approved || err != nil {
		t.Fatalf("first poll = %v/%v, want pending/nil", approved, err)
	}
	// RFC 8628 §3.5: the client polls at the issuer-provided interval — a
	// second call inside the 5 s window must NOT hit the issuer again.
	if approved, _, err := sess.Poll(context.Background()); approved || err != nil {
		t.Fatalf("throttled poll = %v/%v, want pending/nil", approved, err)
	}
	if tokenHits.Load() != 1 {
		t.Errorf("token polls = %d, want 1 — the interval window must throttle issuer calls", tokenHits.Load())
	}
}

func TestStepwiseDevice_PollApprovedExchangesAndPersists(t *testing.T) {
	var polls atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"device_auth_id":"dev-1","user_code":"WDJB","interval":"1"}`)
		case "/api/accounts/deviceauth/token":
			if polls.Add(1) < 2 {
				w.WriteHeader(http.StatusForbidden) // X20: pending
				return
			}
			w.Header().Set("Content-Type", "application/json")
			// X20: the SERVER provides the PKCE pair.
			_, _ = io.WriteString(w, `{"authorization_code":"ac-9","code_verifier":"ver-9"}`)
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Errorf("exchange must be form-encoded (X16): %v", err)
			}
			if got := r.Form.Get("redirect_uri"); got != srv.URL+"/deviceauth/callback" {
				t.Errorf("exchange redirect_uri = %q, want {issuer}/deviceauth/callback (X20)", got)
			}
			if got := r.Form.Get("code_verifier"); got != "ver-9" {
				t.Errorf("exchange code_verifier = %q, want the server-provided ver-9 (X20)", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"access_token":"a1","refresh_token":"r1","id_token":%q}`+"\n", makeJWT(t, "acct-9"))
		default:
			t.Errorf("unexpected issuer path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	store := freshStore()
	auth := testAuth(t, srv.URL, store)
	sess, err := auth.StartDeviceLogin(context.Background(), time.Minute)
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	if approved, _, err := sess.Poll(context.Background()); approved || err != nil {
		t.Fatalf("first poll = %v/%v, want pending/nil", approved, err)
	}
	// Interval is 1 s — the second poll rides after the window.
	time.Sleep(1100 * time.Millisecond)
	approved, res, err := sess.Poll(context.Background())
	if err != nil {
		t.Fatalf("approval poll: %v", err)
	}
	if !approved {
		t.Fatal("approved = false on the 200 poll response, want true (X20)")
	}
	if res.AccountID != "acct-9" {
		t.Errorf("AccountID = %q, want acct-9 from the id_token (X15)", res.AccountID)
	}
	if got := store.get(codex.KindAccessToken); got != "a1" {
		t.Errorf("access_token = %q, want a1 persisted", got)
	}
	if got := store.get(codex.KindRefreshToken); got != "r1" {
		t.Errorf("refresh_token = %q, want r1 persisted", got)
	}
}

func TestStepwiseDevice_PollAfterApprovalIsTerminal(t *testing.T) {
	var tokenHits, exchangeHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"device_auth_id":"dev-1","user_code":"WDJB","interval":"1"}`)
		case "/api/accounts/deviceauth/token":
			tokenHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"authorization_code":"ac","code_verifier":"v"}`)
		case "/oauth/token":
			exchangeHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"a1","refresh_token":"r1"}`)
		default:
			t.Errorf("unexpected issuer path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	store := freshStore()
	auth := testAuth(t, srv.URL, store)
	sess, err := auth.StartDeviceLogin(context.Background(), time.Minute)
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	if approved, _, err := sess.Poll(context.Background()); !approved || err != nil {
		t.Fatalf("first poll = %v/%v, want approved/nil", approved, err)
	}
	// A re-poll after completion must not re-run the (single-use) flow.
	if _, _, err := sess.Poll(context.Background()); err == nil {
		t.Fatal("re-poll after approval must error — the authorization code is single-use")
	}
	if tokenHits.Load() != 1 || exchangeHits.Load() != 1 {
		t.Errorf("hits after re-poll = token %d / exchange %d, want 1/1 — no second round trip", tokenHits.Load(), exchangeHits.Load())
	}
}

func TestStepwiseDevice_PollFatalStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"device_auth_id":"dev-1","user_code":"WDJB","interval":"1"}`)
		case "/api/accounts/deviceauth/token":
			w.WriteHeader(http.StatusInternalServerError) // X20: not 403/404 → fatal
		default:
			t.Errorf("unexpected issuer path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	store := freshStore()
	auth := testAuth(t, srv.URL, store)
	sess, err := auth.StartDeviceLogin(context.Background(), time.Minute)
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	if approved, _, err := sess.Poll(context.Background()); approved || err == nil {
		t.Fatal("500 on the poll must be fatal (X20)")
	}
}

func TestStepwiseDevice_PollPastDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"device_auth_id":"dev-1","user_code":"WDJB","interval":"1"}`)
		case "/api/accounts/deviceauth/token":
			t.Error("the poll must not reach the issuer past the deadline (X20 cap)")
		default:
			t.Errorf("unexpected issuer path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	store := freshStore()
	auth := testAuth(t, srv.URL, store)
	// A 1 ns timeout is in the past by the time Poll runs — the X20 hard cap
	// applies to the stepwise flow the same way it does to the blocking one.
	// (The sleep rides out the coarse Windows wall-clock tick so the deadline
	// is provably past.)
	sess, err := auth.StartDeviceLogin(context.Background(), time.Nanosecond)
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	if _, _, err := sess.Poll(context.Background()); err == nil {
		t.Fatal("poll past the deadline must fail (X20 cap)")
	}
}

func TestStepwiseDevice_DisabledIsOperatorError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// X20: 404 on the INITIAL usercode request = device login disabled.
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	store := freshStore()
	auth := testAuth(t, srv.URL, store)
	_, err := auth.StartDeviceLogin(context.Background(), time.Minute)
	var op *codex.DeviceLoginDisabledError
	if !errors.As(err, &op) {
		t.Fatalf("error = %v (%T), want *codex.DeviceLoginDisabledError (X20)", err, err)
	}
}

// --- helpers ------------------------------------------------------------------

// seedRefreshable plants the minimal refreshable state (refresh token only).
func seedRefreshable(store codex.TokenStore, refresh string) error {
	return store.PutProviderTokens(context.Background(),
		storage.ProviderToken{ProviderID: providerID, Kind: codex.KindRefreshToken, Secret: []byte(refresh)},
	)
}

// simulateBrowser parses the authorize URL, then GETs the redirect_uri with
// code (+ optional wrong-state) as the real browser redirect would.
func simulateBrowser(t *testing.T, authorizeURL string, code string, wrongState ...string) error {
	t.Helper()
	u, err := url.Parse(authorizeURL)
	if err != nil {
		return err
	}
	callback := u.Query().Get("redirect_uri")
	state := u.Query().Get("state")
	if len(wrongState) > 0 {
		state = wrongState[0]
	}
	cb, err := url.Parse(callback)
	if err != nil {
		return err
	}
	q := cb.Query()
	q.Set("code", code)
	q.Set("state", state)
	cb.RawQuery = q.Encode()
	resp, err := http.Get(cb.String()) // test-only URL built from the authorize params
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // test helper
	return nil
}

// sha256B64URL is the test-side S256 reference implementation.
func sha256B64URL(b []byte) string {
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
