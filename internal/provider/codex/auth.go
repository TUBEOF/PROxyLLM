// OAuth client implementing the Auth interface (plan §6.3). Wire facts derive
// from the live-researched codex-rs sources (digest §5b, accessed
// 2026-09-11/12): authorize URL X17, PKCE X21, form exchange X16, JSON refresh
// grant X9, single-use rotation X11/X12, device flow X20, lazy refresh X18.
package codex

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tubeof/proxyllm/internal/httpguard"
	"github.com/tubeof/proxyllm/internal/storage"
)

const (
	// OAuthClientID is the pinned codex CLI OAuth client ID (X13,
	// codex-rs/login/src/auth/manager.rs); operators may override it via
	// AuthConfig.ClientID.
	OAuthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	// Token-store kinds mirroring the codex auth.json semantics (X6).
	KindAccessToken  = "access_token"
	KindIDToken      = "id_token"
	KindRefreshToken = "refresh_token"

	// refreshInterval mirrors the codex CLI's REFRESH_INTERVAL (X18): a
	// successful rotation marks the tokens fresh for this long. Refresh calls
	// inside the window are satisfied without another issuer round trip, so
	// the concurrent late callers of a rotation reuse its result instead of
	// racing the now-consumed refresh token (X12 single-flight).
	refreshInterval = 8 * time.Minute

	// refreshWindow is the pre-expiry lazy refresh window (X18: the CLI
	// refreshes access tokens 5 minutes before expiry). AccessToken rotates
	// when the stored token enters this window.
	refreshWindow = 5 * time.Minute

	// accessTTLDefault stamps the persisted access-token expiry: the token
	// responses carry no expires_in (X10), so PROxyLLM applies a conservative
	// TTL and refreshes lazily on it (X18).
	accessTTLDefault = 15 * time.Minute
)

// The refresh cadence must keep covering the lazy window: AccessToken
// short-circuits Refresh inside refreshInterval (X18), so the interval plus
// the pre-expiry window must never exceed the stamped access-token TTL —
// otherwise a caller entering the lazy window could skip a rotation whose
// result would already be expired again. Constant-coupling guard, checked
// once per process.
func init() {
	if refreshInterval+refreshWindow > accessTTLDefault {
		panic("codex: refreshInterval+refreshWindow must stay <= accessTTLDefault")
	}
}

// TokenStore is the persistence surface the OAuth client needs; storage.Store
// satisfies it. One PutProviderTokens call is one transaction — X12 atomic
// rotation rides on that.
type TokenStore interface {
	PutProviderTokens(ctx context.Context, tokens ...storage.ProviderToken) error
	GetProviderToken(ctx context.Context, providerID, kind string) (*storage.ProviderToken, error)
}

// SessionToken is the per-request backend bearer material: the OAuth access
// token plus the ChatGPT account ID from the id_token claim (X15).
type SessionToken struct {
	Access    string
	AccountID string
}

// LoginResult reports the outcome of a login flow.
type LoginResult struct {
	AccountID string
}

// BrowserLogin parameterizes the interactive browser PKCE flow.
type BrowserLogin struct {
	// BindAddr is the callback listener address — loopback only. Anything
	// else is refused (Analysis G control 7).
	BindAddr string
	// OpenURL opens the authorize URL for the operator (browser launch).
	// nil means the operator drives the URL themselves.
	OpenURL func(authorizeURL string) error
	// Timeout caps the whole flow (listener + exchange). Default 5 min.
	Timeout time.Duration
}

// DeviceLogin parameterizes the headless device-code flow (X20).
type DeviceLogin struct {
	// OpenURL presents the verification URL to the operator. nil = skip.
	OpenURL func(url string) error
	// Timeout is the hard overall cap (CLI: 15 min, X20). Default 15 min.
	Timeout time.Duration
}

// Auth is the OAuth surface the provider consumes.
type Auth interface {
	// BuildAuthorizeURL builds the authorize URL (X17) and returns it with
	// the PKCE verifier for the exchange.
	BuildAuthorizeURL(state string, port int) (authorizeURL string, verifier string, err error)
	// LoginFromBrowser runs the interactive browser PKCE flow (X1/X3/X17).
	LoginFromBrowser(ctx context.Context, cfg BrowserLogin) (LoginResult, error)
	// LoginFromDevice runs the headless device-code flow (X20).
	LoginFromDevice(ctx context.Context, cfg DeviceLogin) (LoginResult, error)
	// StartDeviceLogin starts the stepwise device flow (X20) — the admin-API
	// form: it surfaces the user_code and verification URL so a UI can show
	// them, and the returned session is driven one poll at a time.
	StartDeviceLogin(ctx context.Context, timeout time.Duration) (*DeviceSession, error)
	// Refresh rotates the token pair server-side (X9/X11/X12): single-use
	// rotation persisted atomically, single-flight.
	Refresh(ctx context.Context) error
	// AccessToken returns the per-request bearer material, refreshing first
	// when the stored access token is expired or near expiry (X18 lazy).
	AccessToken(ctx context.Context) (SessionToken, error)
}

// AuthConfig wires the OAuth client.
type AuthConfig struct {
	// Issuer is the OAuth issuer root, e.g. https://auth.openai.com (X1).
	Issuer string
	// ClientID defaults to OAuthClientID (X13) when empty.
	ClientID string
	// HTTP is the issuer-facing HTTP client; defaults to a guarded client
	// (redirects re-validated against the private-address SSRF guard).
	HTTP *http.Client
	// Store persists the token triple (encrypted in production; X12/OA-2).
	Store TokenStore
	// ProviderID keys the token-store rows; defaults to "codex". Production
	// wiring passes the registry provider ID so provider rows and tokens
	// align.
	ProviderID string
}

// refreshFlight is one in-flight (or just-completed) refresh round: waiters
// block on done and read err, which is written before the close (X12
// single-flight).
type refreshFlight struct {
	done chan struct{}
	err  error
}

// authClient is the concrete Auth implementation.
type authClient struct {
	issuer     string
	clientID   string
	httpc      *http.Client
	store      TokenStore
	providerID string

	mu          sync.Mutex
	flight      *refreshFlight // non-nil while a refresh is in flight
	lastRefresh time.Time      // last successful rotation (X18 cadence)
}

// NewAuthClient builds the OAuth client.
func NewAuthClient(cfg AuthConfig) Auth {
	clientID := cfg.ClientID
	if clientID == "" {
		clientID = OAuthClientID // X13
	}
	httpc := cfg.HTTP
	if httpc == nil {
		// OAuth token exchanges: bounded round trips, but redirects
		// re-validated against the private-address SSRF guard — the auth
		// endpoints take operator-configured issuers.
		httpc = httpguard.NewClient(0)
	}
	providerID := cfg.ProviderID
	if providerID == "" {
		providerID = "codex"
	}
	return &authClient{
		issuer:     strings.TrimSuffix(cfg.Issuer, "/"),
		clientID:   clientID,
		httpc:      httpc,
		store:      cfg.Store,
		providerID: providerID,
	}
}

var _ Auth = (*authClient)(nil)

// BuildAuthorizeURL builds the authorize URL per X17: explicit
// code_challenge_method=S256, the codex scope set, originator (X14), and the
// simplified-flow/organizations flags. The returned verifier belongs to the
// challenge in the URL (X21).
func (a *authClient) BuildAuthorizeURL(state string, port int) (string, string, error) {
	verifier, err := NewVerifier() // X21: 64 bytes, base64url unpadded
	if err != nil {
		return "", "", err
	}
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", a.clientID)
	q.Set("redirect_uri", fmt.Sprintf("http://localhost:%d/auth/callback", port))
	q.Set("scope", "openid profile email offline_access api.connectors.read api.connectors.invoke")
	q.Set("code_challenge", Challenge(verifier))
	q.Set("code_challenge_method", "S256") // X17: sent as its own param
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("state", state)
	q.Set("originator", "codex_cli_rs") // X14
	return a.issuer + "/oauth/authorize?" + q.Encode(), verifier, nil
}

// LoginFromBrowser runs the interactive flow: loopback callback listener
// (refusing anything else, Analysis G control 7) → authorize URL (X17) →
// state-validated callback → form-encoded exchange (X16) → atomic persist.
func (a *authClient) LoginFromBrowser(ctx context.Context, cfg BrowserLogin) (LoginResult, error) {
	host, _, err := net.SplitHostPort(cfg.BindAddr)
	if err != nil {
		return LoginResult{}, fmt.Errorf("codex: parse callback bind address %q: %w", cfg.BindAddr, err)
	}
	if host != "localhost" && !net.ParseIP(host).IsLoopback() {
		// Analysis G control 7: the callback listener binds loopback only —
		// never a wildcard address. Refused synchronously, before listening.
		return LoginResult{}, fmt.Errorf("codex: refusing non-loopback callback bind %q (Analysis G control 7)", cfg.BindAddr)
	}
	ln, err := net.Listen("tcp", cfg.BindAddr)
	if err != nil {
		return LoginResult{}, fmt.Errorf("codex: bind callback listener: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	state, err := randomState()
	if err != nil {
		_ = ln.Close()
		return LoginResult{}, err
	}
	authorizeURL, verifier, err := a.BuildAuthorizeURL(state, port)
	if err != nil {
		_ = ln.Close()
		return LoginResult{}, err
	}

	// One-shot callback capture: the handler answers the browser first, then
	// signals the flow (the response must be on the wire before we tear the
	// listener down). The server MUST be serving before OpenURL runs — the
	// "browser" hits the callback as soon as it is opened, and an un-served
	// listener accepts nothing (the connection idles in the kernel backlog
	// and the browser GET never gets its response).
	type callback struct{ code, state string }
	cbCh := make(chan callback, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusForbidden)
		} else {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "Login complete — you may close this window.")
		}
		select {
		case cbCh <- callback{code: q.Get("code"), state: q.Get("state")}:
		default: // a second (retry) request must never block the handler
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	if cfg.OpenURL != nil {
		if err := cfg.OpenURL(authorizeURL); err != nil {
			return LoginResult{}, fmt.Errorf("codex: open authorize URL: %w", err)
		}
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var cb callback
	select {
	case cb = <-cbCh:
	case <-timer.C:
		return LoginResult{}, fmt.Errorf("codex: browser login timed out after %s", timeout)
	case <-ctx.Done():
		return LoginResult{}, ctx.Err()
	case err := <-srvErr:
		return LoginResult{}, fmt.Errorf("codex: callback listener: %w", err)
	}
	if cb.state != state {
		return LoginResult{}, errors.New("codex: callback state mismatch — login aborted before the token exchange")
	}
	if cb.code == "" {
		return LoginResult{}, errors.New("codex: callback carried no authorization code")
	}

	// The exchange redirect_uri must exactly match the authorize-URL one
	// (OA-1: exact redirect-URI matching).
	redirectURI := fmt.Sprintf("http://localhost:%d/auth/callback", port)
	res, err := a.exchangeCode(ctx, cb.code, redirectURI, verifier)
	if err != nil {
		return LoginResult{}, err
	}
	a.markRefreshed()
	return res, nil
}

// LoginFromDevice runs the headless flow per X20 by delegating to the
// stepwise primitives (StartDeviceLogin/Poll): usercode request (404 =
// disabled → operator error) → operator verification URL → poll loop with
// 403/404 tolerance under a hard deadline → exchange with the SERVER-provided
// PKCE verifier and the issuer device callback redirect_uri.
func (a *authClient) LoginFromDevice(ctx context.Context, cfg DeviceLogin) (LoginResult, error) {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Minute // X20 hard cap (CLI: 15 min)
	}

	sess, err := a.StartDeviceLogin(ctx, timeout)
	if err != nil {
		return LoginResult{}, err
	}

	if cfg.OpenURL != nil {
		if err := cfg.OpenURL(sess.VerificationURL); err != nil {
			return LoginResult{}, fmt.Errorf("codex: present verification URL: %w", err)
		}
	}

	// The overall deadline rides the context too, so EVERY request of the
	// flow — usercode, polls, exchange — is bounded even if a caller passes
	// an unbounded ctx.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Poll loop: the session throttles to the issuer interval (RFC 8628
	// §3.5); the loop only adds deadline/cancellation waiting between polls.
	for {
		approved, res, err := sess.Poll(ctx)
		if err != nil {
			return LoginResult{}, err
		}
		if approved {
			return res, nil
		}
		wait := min(sess.Interval, time.Until(sess.deadline))
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return LoginResult{}, ctx.Err()
		}
	}
}

// Refresh rotates the token pair (X9/X11/X12): single-flight — concurrent
// callers coalesce around one issuer round trip and share its result — and a
// recent successful rotation satisfies subsequent callers within the X18
// cadence window, so a serialized caller never races the consumed token.
func (a *authClient) Refresh(ctx context.Context) error {
	a.mu.Lock()
	if !a.lastRefresh.IsZero() && time.Since(a.lastRefresh) < refreshInterval {
		a.mu.Unlock()
		return nil // tokens rotated recently (X18 cadence)
	}
	if a.flight != nil {
		f := a.flight
		a.mu.Unlock()
		<-f.done
		return f.err
	}
	f := &refreshFlight{done: make(chan struct{})}
	a.flight = f
	a.mu.Unlock()

	f.err = a.doRefresh(ctx)
	if f.err == nil {
		a.mu.Lock()
		a.lastRefresh = time.Now()
		a.mu.Unlock()
	}
	close(f.done)

	a.mu.Lock()
	if a.flight == f {
		a.flight = nil
	}
	a.mu.Unlock()
	return f.err
}

// AccessToken returns the per-request bearer material (X15/X18). The store is
// read fresh each call so operator/logins from other flows are picked up
// without reconstruction; an expired or near-expiry token rotates BEFORE the
// request rides (lazy refresh, X18).
func (a *authClient) AccessToken(ctx context.Context) (SessionToken, error) {
	tok, err := a.store.GetProviderToken(ctx, a.providerID, KindAccessToken)
	if err == nil && !needsRefresh(tok) {
		return SessionToken{Access: string(tok.Secret), AccountID: a.accountID(ctx)}, nil
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return SessionToken{}, fmt.Errorf("codex: read access token: %w", err)
	}
	// Missing or inside the expiry window: rotate first (X18).
	if err := a.Refresh(ctx); err != nil {
		return SessionToken{}, err
	}
	tok, err = a.store.GetProviderToken(ctx, a.providerID, KindAccessToken)
	if err != nil {
		return SessionToken{}, fmt.Errorf("codex: read access token after refresh: %w", err)
	}
	return SessionToken{Access: string(tok.Secret), AccountID: a.accountID(ctx)}, nil
}

// needsRefresh reports whether the access token is inside the lazy refresh
// window (X18). A token without expiry info is trusted as-is (review NOTE:
// conservative default — tokens persisted by PROxyLLM ALWAYS carry the
// stamped accessTTLDefault, so nil-ExpiresAt only occurs for externally
// seeded rows; refreshing those unprompted could consume an unknown
// refresh token).
func needsRefresh(tok *storage.ProviderToken) bool {
	if tok.ExpiresAt == nil {
		return false
	}
	return time.Until(time.Unix(*tok.ExpiresAt, 0)) < refreshWindow
}

// accountID reads the id_token fresh and extracts the chatgpt_account_id claim
// (X15). Failures degrade to no account header — the bearer still rides.
func (a *authClient) accountID(ctx context.Context) string {
	tok, err := a.store.GetProviderToken(ctx, a.providerID, KindIDToken)
	if err != nil {
		return ""
	}
	id, err := accountIDFromJWT(string(tok.Secret))
	if err != nil {
		return ""
	}
	return id
}

// markRefreshed records a successful rotation (X18 cadence window).
func (a *authClient) markRefreshed() {
	a.mu.Lock()
	a.lastRefresh = time.Now()
	a.mu.Unlock()
}

// doRefresh performs one refresh round: JSON grant (X9), single-use rotation
// persisted atomically (X11/X12), operator-facing re-login error on
// consumed/expired/revoked tokens — the pre-failure pair stays untouched.
func (a *authClient) doRefresh(ctx context.Context) error {
	refreshTok, err := a.store.GetProviderToken(ctx, a.providerID, KindRefreshToken)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return &ReLoginRequiredError{Reason: "no stored refresh token — run a login first"}
		}
		return fmt.Errorf("codex: read refresh token: %w", err)
	}
	var resp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	err = a.postJSON(ctx, a.issuer+"/oauth/token", struct {
		ClientID     string `json:"client_id"`
		GrantType    string `json:"grant_type"`
		RefreshToken string `json:"refresh_token"`
	}{
		ClientID:     a.clientID,
		GrantType:    "refresh_token",
		RefreshToken: string(refreshTok.Secret),
	}, &resp)
	if err != nil {
		var ee *endpointError
		if errors.As(err, &ee) {
			switch ee.code {
			case "refresh_token_reused", "refresh_token_expired", "refresh_token_invalidated":
				// X11/X12: consumed elsewhere, expired, or revoked — the
				// operator must re-login. Never a silent retry loop.
				return &ReLoginRequiredError{Reason: ee.detail()}
			}
		}
		return err
	}
	if resp.AccessToken == "" {
		return errors.New("codex: refresh response missing access_token")
	}
	// X10: the response fields are optional. Keep the stored refresh token
	// when the response omits one (rotation persists when present — X11).
	refresh := string(refreshTok.Secret)
	if resp.RefreshToken != "" {
		refresh = resp.RefreshToken
	}
	return a.persistTokens(ctx, resp.AccessToken, refresh, resp.IDToken)
}

// exchangeCode performs the authorization-code grant (X16: form-encoded) and
// persists the resulting token triple atomically.
func (a *authClient) exchangeCode(ctx context.Context, code, redirectURI, codeVerifier string) (LoginResult, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", a.clientID)
	form.Set("code_verifier", codeVerifier)

	var resp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := a.postForm(ctx, a.issuer+"/oauth/token", form, &resp); err != nil {
		return LoginResult{}, err
	}
	if resp.AccessToken == "" {
		return LoginResult{}, errors.New("codex: token exchange response missing access_token")
	}
	accountID := ""
	if resp.IDToken != "" {
		var err error
		if accountID, err = accountIDFromJWT(resp.IDToken); err != nil {
			return LoginResult{}, err
		}
	}
	if err := a.persistTokens(ctx, resp.AccessToken, resp.RefreshToken, resp.IDToken); err != nil {
		return LoginResult{}, err
	}
	return LoginResult{AccountID: accountID}, nil
}

// persistTokens writes the token set in ONE store call — one transaction, so
// rotation is atomic (X12). The access token carries the conservative TTL
// (the issuer sends no expires_in, X10) and a last-refresh stamp.
func (a *authClient) persistTokens(ctx context.Context, access, refresh, idToken string) error {
	now := time.Now()
	expires := now.Add(accessTTLDefault).Unix()
	lastRefresh := now.Unix()
	tokens := []storage.ProviderToken{{
		ProviderID:  a.providerID,
		Kind:        KindAccessToken,
		Secret:      []byte(access),
		ExpiresAt:   &expires,
		LastRefresh: &lastRefresh,
	}}
	if refresh != "" {
		tokens = append(tokens, storage.ProviderToken{
			ProviderID: a.providerID,
			Kind:       KindRefreshToken,
			Secret:     []byte(refresh),
		})
	}
	if idToken != "" {
		tokens = append(tokens, storage.ProviderToken{
			ProviderID: a.providerID,
			Kind:       KindIDToken,
			Secret:     []byte(idToken),
		})
	}
	return a.store.PutProviderTokens(ctx, tokens...)
}

// endpointError is a non-2xx issuer response with the decoded OAuth error
// fields (X11 failure codes ride `error`).
type endpointError struct {
	status      int
	code        string
	description string
	body        string
}

func (e *endpointError) Error() string {
	if e.code != "" {
		return fmt.Sprintf("codex: issuer endpoint: status %d: %s (%s)", e.status, e.description, e.code)
	}
	// The raw body is issuer-controlled text and may echo request material —
	// never let an unbounded body ride an error string (redaction layer
	// baseline: log-safe by construction).
	return fmt.Sprintf("codex: issuer endpoint: status %d: %s", e.status, truncateForLog(e.body, 256))
}

// truncateForLog bounds an issuer-provided string and marks the cut.
func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…(truncated)"
}

// detail renders the failure reason for operator-facing errors.
func (e *endpointError) detail() string {
	if e.description != "" {
		return e.description
	}
	return e.code
}

// postForm sends a form-encoded request (X16 exchange).
func (a *authClient) postForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("codex: build issuer request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return a.do(req, out)
}

// postJSON sends a JSON request (X9 refresh grant, X20 device flow).
func (a *authClient) postJSON(ctx context.Context, endpoint string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("codex: encode issuer request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("codex: build issuer request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return a.do(req, out)
}

// do executes the request, decoding a 2xx into out (nil = discard) and
// mapping non-2xx bodies onto *endpointError.
func (a *authClient) do(req *http.Request, out any) error {
	resp, err := a.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("codex: call %s: %w", req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		ee := &endpointError{status: resp.StatusCode, body: strings.TrimSpace(string(raw))}
		var oe struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		if json.Unmarshal(raw, &oe) == nil && oe.Error != "" {
			ee.code = oe.Error
			// error_description is issuer-controlled and unbounded — bound it
			// at construction so every consumer (Error string, re-login
			// reason) is log-safe by construction.
			ee.description = truncateForLog(oe.Description, 256)
		}
		return ee
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("codex: decode %s response: %w", req.URL.Path, err)
	}
	return nil
}

// randomState generates the CSRF state for the browser flow.
func randomState() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("codex: read state entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// accountIDFromJWT extracts the chatgpt_account_id claim from the id_token
// payload (X15). The token is not verified here — it arrives over TLS
// directly from the issuer; the claim only names the account header.
func accountIDFromJWT(idToken string) (string, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return "", errors.New("codex: id_token is not a three-part JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("codex: decode id_token payload: %w", err)
	}
	var claims struct {
		ChatgptAccountID string `json:"chatgpt_account_id"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("codex: parse id_token claims: %w", err)
	}
	return claims.ChatgptAccountID, nil
}
