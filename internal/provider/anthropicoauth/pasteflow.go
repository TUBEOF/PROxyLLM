package anthropicoauth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	// pasteTTL bounds a pending paste flow.
	pasteTTL = 15 * time.Minute
)

// Errors of the paste completion path.
var (
	// ErrFlowNotFound means the flow id is unknown or already consumed.
	ErrFlowNotFound = errors.New("anthropicoauth: login flow not found or expired")
	// ErrFlowExpired means the flow outlived its TTL.
	ErrFlowExpired = errors.New("anthropicoauth: login flow expired — start a new sign-in")
	// ErrStateMismatch means the pasted state does not belong to the flow
	// (login-CSRF protection).
	ErrStateMismatch = errors.New("anthropicoauth: state mismatch — the pasted callback does not belong to this sign-in")
	// ErrCodeMissing means the input carried no authorization code.
	ErrCodeMissing = errors.New("anthropicoauth: no authorization code in the pasted input")
)

// PasteFlow is the server-side sign-in the console shows: the flow id, the
// authorize URL to open, and the redirect URI the flow is bound to.
type PasteFlow struct {
	ID           string    `json:"flow_id"`
	AuthorizeURL string    `json:"authorize_url"`
	RedirectURI  string    `json:"redirect_uri"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// pasteFlow is one pending sign-in. The verifier never leaves the server.
type pasteFlow struct {
	id          string
	state       string
	verifier    string
	redirectURI string
	expires     time.Time
}

// StartPasteLogin creates the flow: a state, a PKCE verifier and the authorize
// URL for the configured loopback redirect. Starting a new flow replaces the
// pending one (one active flow per provider).
func (a *authClient) StartPasteLogin(_ context.Context) (PasteFlow, error) {
	state, err := newState()
	if err != nil {
		return PasteFlow{}, err
	}
	verifier, err := newVerifier()
	if err != nil {
		return PasteFlow{}, err
	}
	id, err := newState() // a second random value; the UI sees only this
	if err != nil {
		return PasteFlow{}, err
	}
	authorizeURL, err := a.buildAuthorizeURLWithVerifier(state, verifier, a.redirectPort)
	if err != nil {
		return PasteFlow{}, err
	}
	f := &pasteFlow{
		id:          id,
		state:       state,
		verifier:    verifier,
		redirectURI: a.redirectURI(a.redirectPort),
		expires:     time.Now().Add(pasteTTL),
	}
	a.pasteMu.Lock()
	a.paste = map[string]*pasteFlow{id: f}
	a.pasteMu.Unlock()
	return PasteFlow{ID: id, AuthorizeURL: authorizeURL, RedirectURI: f.redirectURI, ExpiresAt: f.expires}, nil
}

// CompletePasteLogin validates the pasted callback against the flow and
// exchanges the code. The flow is single-use: any completion attempt consumes
// it, so a wrong state cannot be retried against the same flow.
func (a *authClient) CompletePasteLogin(ctx context.Context, flowID, input string) (LoginResult, error) {
	a.pasteMu.Lock()
	f, ok := a.paste[flowID]
	if ok {
		delete(a.paste, flowID)
	}
	a.pasteMu.Unlock()
	if !ok {
		return LoginResult{}, ErrFlowNotFound
	}
	if time.Now().After(f.expires) {
		return LoginResult{}, ErrFlowExpired
	}
	code, state, err := parseCallbackInput(input)
	if err != nil {
		return LoginResult{}, err
	}
	if state == "" {
		// A bare code is only accepted through the server-issued flow id,
		// which is itself an unguessable capability; it still binds to the
		// flow's state below.
		state = f.state
	}
	if state != f.state {
		return LoginResult{}, ErrStateMismatch
	}
	res, err := a.exchangeCode(ctx, code, state, f.redirectURI, f.verifier)
	if err != nil {
		return LoginResult{}, err
	}
	a.mu.Lock()
	a.lastRefresh = time.Now()
	a.mu.Unlock()
	return res, nil
}

// buildAuthorizeURLWithVerifier is BuildAuthorizeURL with a supplied verifier
// (the paste flow must keep the verifier server-side from the start).
func (a *authClient) buildAuthorizeURLWithVerifier(state, verifier string, port int) (string, error) {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", a.clientID)
	q.Set("redirect_uri", a.redirectURI(port))
	q.Set("scope", a.scope)
	q.Set("code_challenge", challenge(verifier))
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	return a.authorizeURL + "?" + q.Encode(), nil
}

// parseCallbackInput accepts the three documented paste forms:
//
//  1. a full callback URL — http://localhost:<port>/callback?code=…&state=…
//  2. the CLI's `code#state` form
//  3. a bare code (bound to the server-issued flow id, see CompletePasteLogin)
//
// A URL or `code#state` with a missing state is rejected: a callback that
// cannot be bound to the flow is a login-CSRF attempt, not a fallback.
func parseCallbackInput(input string) (code, state string, err error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", "", ErrCodeMissing
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return "", "", fmt.Errorf("anthropicoauth: parse callback URL: %w", ErrCodeMissing)
		}
		q := u.Query()
		code, state = q.Get("code"), q.Get("state")
		if code == "" {
			return "", "", ErrCodeMissing
		}
		if state == "" {
			return "", "", ErrStateMismatch
		}
		return code, state, nil
	}
	if before, after, ok := strings.Cut(s, "#"); ok {
		if before == "" {
			return "", "", ErrCodeMissing
		}
		if after == "" {
			return "", "", ErrStateMismatch
		}
		return before, after, nil
	}
	return s, "", nil
}
