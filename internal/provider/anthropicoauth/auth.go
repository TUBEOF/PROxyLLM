// Package anthropicoauth is the Anthropic OAuth provider client (ADR-022):
// browser PKCE with a loopback callback and a paste-code completion path,
// server-side refresh with single-use rotation, and a Bearer + oauth beta
// header for inference.
//
// Wire facts and their provenance live in docs/research/anthropic-oauth.md.
// Consumer (Free/Pro/Max) OAuth tokens are rejected by Anthropic for
// third-party use; this provider targets Teams/Enterprise workspaces only.
package anthropicoauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tubeof/proxyllm/internal/httpguard"
	"github.com/tubeof/proxyllm/internal/storage"
)

const (
	// AuthorizeEndpoint is the documented authorization endpoint.
	AuthorizeEndpoint = "https://claude.ai/oauth/authorize"
	// TokenEndpoint is the documented token endpoint (the older
	// console.anthropic.com/v1/oauth/token is interoperable; it is a
	// configuration override, not a second default).
	TokenEndpoint = "https://platform.claude.com/v1/oauth/token" //nolint:gosec // a public documented endpoint, not a credential
	// OAuthClientID is the Claude Code client id; operators may override it
	// through AuthConfig.ClientID (OQ-1).
	OAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	// DefaultRedirectPort is the documented Claude Code loopback port.
	DefaultRedirectPort = 53692
	// DefaultScope is the minimal chat scope set (OQ-2).
	DefaultScope = "user:profile user:inference"

	// BetaHeader is the Anthropic beta flag that accompanies an OAuth bearer.
	BetaHeader = "oauth-2025-04-20"

	// Token-store kinds.
	KindAccessToken  = "access_token"
	KindRefreshToken = "refresh_token"
	// KindAccountID holds the account email from the token response, for the
	// console's status line. It is never an authorization decision.
	KindAccountID = "account_id"

	// accessTTLDefault stamps the access-token expiry when a token response
	// omits expires_in (the documented value is 28800 s).
	accessTTLDefault = 8 * time.Hour
	// refreshWindow is the pre-expiry lazy refresh window.
	refreshWindow = 5 * time.Minute
	// refreshCadence lets concurrent late callers reuse a just-completed
	// rotation instead of racing a consumed refresh token.
	refreshCadence = time.Minute
)

// TokenStore is the persistence surface the client needs; storage.Store
// satisfies it. One PutProviderTokens call is one transaction.
type TokenStore interface {
	PutProviderTokens(ctx context.Context, tokens ...storage.ProviderToken) error
	GetProviderToken(ctx context.Context, providerID, kind string) (*storage.ProviderToken, error)
}

// SessionToken is the per-request bearer material.
type SessionToken struct {
	Access    string
	AccountID string
}

// LoginResult reports the outcome of a login.
type LoginResult struct {
	AccountID string
}

// BrowserLogin parameterizes the loopback login.
type BrowserLogin struct {
	// BindAddr is the callback listener address — loopback only.
	BindAddr string
	// OpenURL opens the authorize URL for the operator; nil means the
	// operator drives it themselves.
	OpenURL func(authorizeURL string) error
	// Timeout caps the whole flow. Default 5 minutes.
	Timeout time.Duration
}

// Auth is the OAuth surface the provider and admin API consume.
type Auth interface {
	// BuildAuthorizeURL builds the authorize URL with PKCE and state, and
	// returns the verifier bound to its challenge.
	BuildAuthorizeURL(state string, port int) (authorizeURL, verifier string, err error)
	// LoginFromLoopback runs the same-machine browser flow.
	LoginFromLoopback(ctx context.Context, cfg BrowserLogin) (LoginResult, error)
	// StartPasteLogin creates the server-side flow for the paste path.
	StartPasteLogin(ctx context.Context) (PasteFlow, error)
	// CompletePasteLogin parses the pasted callback and exchanges the code.
	CompletePasteLogin(ctx context.Context, flowID, input string) (LoginResult, error)
	// Refresh rotates the token pair, single-flight.
	Refresh(ctx context.Context) error
	// AccessToken returns the bearer material, refreshing before expiry.
	AccessToken(ctx context.Context) (SessionToken, error)
	// HasToken reports whether a credential row exists, without refreshing.
	HasToken(ctx context.Context) bool
}

// AuthConfig wires the client.
type AuthConfig struct {
	AuthorizeURL string
	TokenURL     string
	ClientID     string
	Scope        string
	// RedirectPort is the loopback port used by the paste flow (the browser
	// lands on http://localhost:<port>/callback, which may not exist).
	RedirectPort int
	HTTP         *http.Client
	Store        TokenStore
	ProviderID   string
}

type authClient struct {
	authorizeURL string
	tokenURL     string
	clientID     string
	scope        string
	redirectPort int
	httpc        *http.Client
	store        TokenStore
	providerID   string

	mu          sync.Mutex
	flight      *refreshFlight
	lastRefresh time.Time

	// paste flows: one active per client (one per provider row).
	pasteMu sync.Mutex
	paste   map[string]*pasteFlow
}

type refreshFlight struct {
	done chan struct{}
	err  error
}

// ReLoginRequiredError means the refresh token is gone or was rejected; a
// human and a browser are required.
type ReLoginRequiredError struct{ Reason string }

func (e *ReLoginRequiredError) Error() string {
	return "anthropicoauth: re-login required: " + e.Reason
}

// NewAuthClient builds the OAuth client with the documented defaults.
func NewAuthClient(cfg AuthConfig) Auth {
	a := cfg.AuthorizeURL
	if a == "" {
		a = AuthorizeEndpoint
	}
	t := cfg.TokenURL
	if t == "" {
		t = TokenEndpoint
	}
	c := cfg.ClientID
	if c == "" {
		c = OAuthClientID
	}
	scope := cfg.Scope
	if scope == "" {
		scope = DefaultScope
	}
	port := cfg.RedirectPort
	if port == 0 {
		port = DefaultRedirectPort
	}
	httpc := cfg.HTTP
	if httpc == nil {
		httpc = httpguard.NewClient(0)
	}
	id := cfg.ProviderID
	if id == "" {
		id = "anthropic-oauth"
	}
	return &authClient{
		authorizeURL: strings.TrimSuffix(a, "/"),
		tokenURL:     t,
		clientID:     c,
		scope:        scope,
		redirectPort: port,
		httpc:        httpc,
		store:        cfg.Store,
		providerID:   id,
		paste:        map[string]*pasteFlow{},
	}
}

var _ Auth = (*authClient)(nil)

func (a *authClient) redirectURI(port int) string {
	return fmt.Sprintf("http://localhost:%d/callback", port)
}

// BuildAuthorizeURL builds the authorize URL and returns its PKCE verifier.
func (a *authClient) BuildAuthorizeURL(state string, port int) (string, string, error) {
	if state == "" {
		return "", "", errors.New("anthropicoauth: state is required")
	}
	if port == 0 {
		port = a.redirectPort
	}
	verifier, err := newVerifier()
	if err != nil {
		return "", "", err
	}
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", a.clientID)
	q.Set("redirect_uri", a.redirectURI(port))
	q.Set("scope", a.scope)
	q.Set("code_challenge", challenge(verifier))
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	return a.authorizeURL + "?" + q.Encode(), verifier, nil
}

// Refresh rotates the token pair. Concurrent callers coalesce around one
// issuer round trip and share its result; a just-completed rotation satisfies
// callers inside the cadence window.
func (a *authClient) Refresh(ctx context.Context) error {
	a.mu.Lock()
	if !a.lastRefresh.IsZero() && time.Since(a.lastRefresh) < refreshCadence {
		a.mu.Unlock()
		return nil
	}
	if a.flight != nil {
		f := a.flight
		a.mu.Unlock()
		select {
		case <-f.done:
			return f.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f := &refreshFlight{done: make(chan struct{})}
	a.flight = f
	a.mu.Unlock()

	// The rotation is shared and must outlive the first caller's request.
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	f.err = a.doRefresh(refreshCtx)
	cancel()
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

// AccessToken returns the bearer material, rotating first when the stored
// token is missing or near expiry.
func (a *authClient) AccessToken(ctx context.Context) (SessionToken, error) {
	tok, err := a.store.GetProviderToken(ctx, a.providerID, KindAccessToken)
	if err == nil && !needsRefresh(tok) {
		return SessionToken{Access: string(tok.Secret), AccountID: a.accountID(ctx)}, nil
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return SessionToken{}, fmt.Errorf("anthropicoauth: read access token: %w", err)
	}
	if err := a.Refresh(ctx); err != nil {
		return SessionToken{}, err
	}
	tok, err = a.store.GetProviderToken(ctx, a.providerID, KindAccessToken)
	if err != nil {
		return SessionToken{}, fmt.Errorf("anthropicoauth: read access token after refresh: %w", err)
	}
	return SessionToken{Access: string(tok.Secret), AccountID: a.accountID(ctx)}, nil
}

// HasToken reports whether a credential row exists.
func (a *authClient) HasToken(ctx context.Context) bool {
	if _, err := a.store.GetProviderToken(ctx, a.providerID, KindAccessToken); err == nil {
		return true
	}
	_, err := a.store.GetProviderToken(ctx, a.providerID, KindRefreshToken)
	return err == nil
}

func needsRefresh(tok *storage.ProviderToken) bool {
	if tok.ExpiresAt == nil {
		return false
	}
	return time.Until(time.Unix(*tok.ExpiresAt, 0)) < refreshWindow
}

func (a *authClient) accountID(ctx context.Context) string {
	tok, err := a.store.GetProviderToken(ctx, a.providerID, KindAccountID)
	if err != nil {
		return ""
	}
	return string(tok.Secret)
}

// doRefresh performs one refresh round and persists the rotated pair.
func (a *authClient) doRefresh(ctx context.Context) error {
	refreshTok, err := a.store.GetProviderToken(ctx, a.providerID, KindRefreshToken)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return &ReLoginRequiredError{Reason: "no stored refresh token — run a login first"}
		}
		return fmt.Errorf("anthropicoauth: read refresh token: %w", err)
	}
	var resp tokenResponse
	err = a.postJSON(ctx, a.tokenURL, map[string]any{
		"grant_type":    "refresh_token",
		"refresh_token": string(refreshTok.Secret),
		"client_id":     a.clientID,
	}, &resp)
	if err != nil {
		var ee *endpointError
		if errors.As(err, &ee) && ee.code == "invalid_grant" {
			return &ReLoginRequiredError{Reason: ee.detail()}
		}
		return err
	}
	if resp.AccessToken == "" {
		return errors.New("anthropicoauth: refresh response missing access_token")
	}
	refresh := string(refreshTok.Secret)
	if resp.RefreshToken != "" {
		refresh = resp.RefreshToken
	}
	return a.persist(ctx, resp, refresh)
}

// exchangeCode trades an authorization code for tokens and persists them.
func (a *authClient) exchangeCode(ctx context.Context, code, state, redirectURI, verifier string) (LoginResult, error) {
	var resp tokenResponse
	err := a.postJSON(ctx, a.tokenURL, map[string]any{
		"grant_type":    "authorization_code",
		"code":          code,
		"state":         state,
		"client_id":     a.clientID,
		"redirect_uri":  redirectURI,
		"code_verifier": verifier,
	}, &resp)
	if err != nil {
		return LoginResult{}, err
	}
	if resp.AccessToken == "" {
		return LoginResult{}, errors.New("anthropicoauth: token response missing access_token")
	}
	if err := a.persist(ctx, resp, resp.RefreshToken); err != nil {
		return LoginResult{}, err
	}
	return LoginResult{AccountID: resp.Account.EmailAddress}, nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	Account      struct {
		UUID         string `json:"uuid"`
		EmailAddress string `json:"email_address"`
	} `json:"account"`
	Organization struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"organization"`
}

// persist writes the token set in one store call — one transaction, so a
// rotation is atomic.
func (a *authClient) persist(ctx context.Context, resp tokenResponse, refresh string) error {
	now := time.Now()
	ttl := accessTTLDefault
	if resp.ExpiresIn > 0 {
		ttl = time.Duration(resp.ExpiresIn) * time.Second
	}
	expires := now.Add(ttl).Unix()
	lastRefresh := now.Unix()
	tokens := []storage.ProviderToken{{
		ProviderID:  a.providerID,
		Kind:        KindAccessToken,
		Secret:      []byte(resp.AccessToken),
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
	if resp.Account.EmailAddress != "" {
		tokens = append(tokens, storage.ProviderToken{
			ProviderID: a.providerID,
			Kind:       KindAccountID,
			Secret:     []byte(resp.Account.EmailAddress),
		})
	}
	return a.store.PutProviderTokens(ctx, tokens...)
}

// endpointError is a non-2xx issuer response. Bodies are bounded and never
// carry token material: the fields are issuer-controlled errors.
type endpointError struct {
	status      int
	code        string
	description string
	body        string
}

func (e *endpointError) Error() string {
	if e.code != "" {
		return fmt.Sprintf("anthropicoauth: issuer endpoint: status %d: %s (%s)", e.status, e.description, e.code)
	}
	return fmt.Sprintf("anthropicoauth: issuer endpoint: status %d: %s", e.status, truncateForLog(e.body, 256))
}

func (e *endpointError) detail() string {
	if e.description != "" {
		return e.description
	}
	return e.code
}

func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…(truncated)"
}

func (a *authClient) postJSON(ctx context.Context, endpoint string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("anthropicoauth: encode issuer request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("anthropicoauth: build issuer request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return a.do(req, out)
}

func (a *authClient) do(req *http.Request, out any) error {
	resp, err := a.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("anthropicoauth: call %s: %w", req.URL.Path, err)
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
			ee.description = truncateForLog(oe.Description, 256)
		}
		return ee
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("anthropicoauth: decode %s response: %w", req.URL.Path, err)
	}
	return nil
}
