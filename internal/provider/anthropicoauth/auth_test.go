package anthropicoauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/storage"
)

// --- fakes ---

type fakeStore struct {
	mu   sync.Mutex
	toks map[string]storage.ProviderToken
}

func newFakeStore() *fakeStore { return &fakeStore{toks: map[string]storage.ProviderToken{}} }

func (s *fakeStore) key(pid, kind string) string { return pid + "/" + kind }

func (s *fakeStore) PutProviderTokens(_ context.Context, toks ...storage.ProviderToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range toks {
		s.toks[s.key(t.ProviderID, t.Kind)] = t
	}
	return nil
}

func (s *fakeStore) GetProviderToken(_ context.Context, pid, kind string) (*storage.ProviderToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.toks[s.key(pid, kind)]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return &t, nil
}

func (s *fakeStore) set(kind, secret string, expires *int64) {
	s.toks[s.key("test-provider", kind)] = storage.ProviderToken{
		ProviderID: "test-provider", Kind: kind, Secret: []byte(secret), ExpiresAt: expires,
	}
}

func (s *fakeStore) get(kind string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.toks[s.key("test-provider", kind)].Secret)
}

// issuer is a fake token endpoint.
type issuer struct {
	srv    *httptest.Server
	calls  atomic.Int32
	mu     sync.Mutex
	bodies []map[string]any
	// respond overrides the response body; default is the documented token
	// response fixture shape.
	respond func(w http.ResponseWriter, body map[string]any)
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	is := &issuer{}
	is.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		is.calls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		is.mu.Lock()
		is.bodies = append(is.bodies, body)
		is.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if is.respond != nil {
			is.respond(w, body)
			return
		}
		_, _ = w.Write([]byte(`{"token_type":"Bearer","access_token":"access-1","refresh_token":"refresh-1",` +
			`"expires_in":28800,"scope":"user:inference user:profile",` +
			`"account":{"uuid":"u","email_address":"operator@example.com"}}`))
	}))
	t.Cleanup(is.srv.Close)
	return is
}

func (is *issuer) lastBody() map[string]any {
	is.mu.Lock()
	defer is.mu.Unlock()
	if len(is.bodies) == 0 {
		return nil
	}
	return is.bodies[len(is.bodies)-1]
}

func newClient(t *testing.T, is *issuer, st *fakeStore) *authClient {
	t.Helper()
	return NewAuthClient(AuthConfig{
		AuthorizeURL: "https://claude.ai/oauth/authorize",
		TokenURL:     is.srv.URL + "/token",
		Store:        st,
		ProviderID:   "test-provider",
	}).(*authClient)
}

// --- authorize URL ---

func TestBuildAuthorizeURL_MatchesDocumentedShape(t *testing.T) {
	is := newIssuer(t)
	a := newClient(t, is, newFakeStore())
	const state = "state-123"
	raw, verifier, err := a.BuildAuthorizeURL(state, DefaultRedirectPort)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "claude.ai" || u.Path != "/oauth/authorize" {
		t.Fatalf("authorize URL = %s, want https://claude.ai/oauth/authorize", raw)
	}
	q := u.Query()
	want := map[string]string{
		"response_type":         "code",
		"client_id":             OAuthClientID,
		"redirect_uri":          "http://localhost:53692/callback",
		"scope":                 "user:profile user:inference",
		"code_challenge_method": "S256",
		"state":                 state,
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	if q.Get("code_challenge") != challenge(verifier) {
		t.Error("code_challenge does not match the returned verifier")
	}
	if len(verifier) < 43 || len(verifier) > 128 {
		t.Errorf("verifier length %d outside RFC 7636 43..128", len(verifier))
	}
}

// The fixture records the documented parameter set; our URL must carry the
// same names (values differ by design).
func TestBuildAuthorizeURL_ParameterNamesMatchFixture(t *testing.T) {
	raw, err := readFixture("authorize-url.txt")
	if err != nil {
		t.Fatal(err)
	}
	fixtureURL, err := url.Parse(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	is := newIssuer(t)
	a := newClient(t, is, newFakeStore())
	ours, _, err := a.BuildAuthorizeURL("s", 53692)
	if err != nil {
		t.Fatal(err)
	}
	ou, _ := url.Parse(ours)
	for name := range fixtureURL.Query() {
		if ou.Query().Get(name) == "" {
			t.Errorf("authorize URL missing documented parameter %q", name)
		}
	}
}

// --- paste flow ---

func TestPasteLogin_CompleteWithURL(t *testing.T) {
	is := newIssuer(t)
	st := newFakeStore()
	a := newClient(t, is, st)
	flow, err := a.StartPasteLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cb := flow.RedirectURI + "?code=the-code&state=" + stateOf(t, flow.AuthorizeURL)
	res, err := a.CompletePasteLogin(context.Background(), flow.ID, cb)
	if err != nil {
		t.Fatalf("CompletePasteLogin: %v", err)
	}
	if res.AccountID != "operator@example.com" {
		t.Errorf("AccountID = %q", res.AccountID)
	}
	body := is.lastBody()
	if body["grant_type"] != "authorization_code" || body["code"] != "the-code" {
		t.Errorf("exchange body = %v", body)
	}
	if body["state"] != stateOf(t, flow.AuthorizeURL) {
		t.Error("exchange did not carry the flow state")
	}
	if body["redirect_uri"] != flow.RedirectURI {
		t.Error("exchange redirect_uri differs from the authorize redirect_uri")
	}
	if body["code_verifier"] == "" {
		t.Error("exchange carried no code_verifier")
	}
	if got := st.get(KindRefreshToken); got != "refresh-1" {
		t.Errorf("stored refresh = %q", got)
	}
	if tok, _ := st.GetProviderToken(context.Background(), "test-provider", KindAccessToken); tok == nil || tok.ExpiresAt == nil {
		t.Fatal("access token not persisted with an expiry")
	} else if want := time.Now().Add(28800 * time.Second).Unix(); *tok.ExpiresAt < want-60 || *tok.ExpiresAt > want+60 {
		t.Errorf("access expiry = %d, want ~%d", *tok.ExpiresAt, want)
	}
}

func TestPasteLogin_CompleteWithCodeHashState(t *testing.T) {
	is := newIssuer(t)
	a := newClient(t, is, newFakeStore())
	flow, err := a.StartPasteLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CompletePasteLogin(context.Background(), flow.ID, "the-code#"+stateOf(t, flow.AuthorizeURL)); err != nil {
		t.Fatalf("code#state: %v", err)
	}
}

func TestPasteLogin_CompleteWithBareCodeBindsToFlow(t *testing.T) {
	is := newIssuer(t)
	a := newClient(t, is, newFakeStore())
	flow, err := a.StartPasteLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CompletePasteLogin(context.Background(), flow.ID, "the-code"); err != nil {
		t.Fatalf("bare code: %v", err)
	}
}

func TestPasteLogin_RejectsWrongState(t *testing.T) {
	is := newIssuer(t)
	a := newClient(t, is, newFakeStore())
	flow, _ := a.StartPasteLogin(context.Background())
	if _, err := a.CompletePasteLogin(context.Background(), flow.ID, "code#wrong"); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("err = %v, want ErrStateMismatch", err)
	}
	if is.calls.Load() != 0 {
		t.Error("a state mismatch must not reach the token endpoint")
	}
	// The flow is single-use even after a rejected attempt.
	if _, err := a.CompletePasteLogin(context.Background(), flow.ID, "code"); !errors.Is(err, ErrFlowNotFound) {
		t.Errorf("second attempt err = %v, want ErrFlowNotFound", err)
	}
}

func TestPasteLogin_RejectsURLWithoutState(t *testing.T) {
	is := newIssuer(t)
	a := newClient(t, is, newFakeStore())
	flow, _ := a.StartPasteLogin(context.Background())
	_, err := a.CompletePasteLogin(context.Background(), flow.ID, "http://localhost:53692/callback?code=abc")
	if !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("err = %v, want ErrStateMismatch", err)
	}
}

func TestPasteLogin_UnknownFlow(t *testing.T) {
	is := newIssuer(t)
	a := newClient(t, is, newFakeStore())
	if _, err := a.CompletePasteLogin(context.Background(), "nope", "code"); !errors.Is(err, ErrFlowNotFound) {
		t.Fatalf("err = %v, want ErrFlowNotFound", err)
	}
}

func TestPasteLogin_StartingAFlowReplacesThePendingOne(t *testing.T) {
	is := newIssuer(t)
	a := newClient(t, is, newFakeStore())
	first, _ := a.StartPasteLogin(context.Background())
	if _, err := a.StartPasteLogin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CompletePasteLogin(context.Background(), first.ID, "code"); !errors.Is(err, ErrFlowNotFound) {
		t.Errorf("old flow err = %v, want ErrFlowNotFound", err)
	}
}

// --- refresh ---

func TestRefresh_PersistsRotatedPair(t *testing.T) {
	is := newIssuer(t)
	st := newFakeStore()
	st.set(KindRefreshToken, "old-refresh", nil)
	expired := time.Now().Add(-time.Hour).Unix()
	st.set(KindAccessToken, "old-access", &expired)
	a := newClient(t, is, st)

	if err := a.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := is.lastBody()
	if body["grant_type"] != "refresh_token" || body["refresh_token"] != "old-refresh" || body["client_id"] != OAuthClientID {
		t.Errorf("refresh body = %v", body)
	}
	if got := st.get(KindAccessToken); got != "access-1" {
		t.Errorf("access = %q", got)
	}
	if got := st.get(KindRefreshToken); got != "refresh-1" {
		t.Errorf("rotated refresh = %q, want the response value", got)
	}
	// Cadence: a second call inside the window does not hit the issuer.
	if err := a.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := is.calls.Load(); n != 1 {
		t.Errorf("token endpoint calls = %d, want 1 (refresh cadence)", n)
	}
}

func TestRefresh_SingleFlight(t *testing.T) {
	is := newIssuer(t)
	is.respond = func(w http.ResponseWriter, _ map[string]any) {
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte(`{"access_token":"a2","refresh_token":"r2","expires_in":28800}`))
	}
	st := newFakeStore()
	st.set(KindRefreshToken, "r1", nil)
	a := newClient(t, is, st)
	a.lastRefresh = time.Time{} // defeat the cadence for this test

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = a.Refresh(context.Background())
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	if n := is.calls.Load(); n != 1 {
		t.Errorf("token endpoint calls = %d, want 1 (single-flight)", n)
	}
}

func TestRefresh_InvalidGrantRequiresRelogin(t *testing.T) {
	is := newIssuer(t)
	is.respond = func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token revoked"}`))
	}
	st := newFakeStore()
	st.set(KindRefreshToken, "super-secret-refresh", nil)
	a := newClient(t, is, st)
	err := a.Refresh(context.Background())
	var rl *ReLoginRequiredError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want ReLoginRequiredError", err)
	}
	if strings.Contains(err.Error(), "super-secret-refresh") {
		t.Error("error text leaked the refresh token")
	}
}

func TestAccessToken_RefreshesWhenExpired(t *testing.T) {
	is := newIssuer(t)
	st := newFakeStore()
	expired := time.Now().Add(-time.Minute).Unix()
	st.set(KindAccessToken, "stale", &expired)
	st.set(KindRefreshToken, "r1", nil)
	a := newClient(t, is, st)
	tok, err := a.AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "access-1" {
		t.Errorf("access = %q, want the refreshed token", tok.Access)
	}
	if n := is.calls.Load(); n != 1 {
		t.Errorf("token endpoint calls = %d, want 1", n)
	}
}

func TestAccessToken_NoRefreshWhenFresh(t *testing.T) {
	is := newIssuer(t)
	st := newFakeStore()
	future := time.Now().Add(time.Hour).Unix()
	st.set(KindAccessToken, "fresh", &future)
	a := newClient(t, is, st)
	tok, err := a.AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "fresh" || is.calls.Load() != 0 {
		t.Errorf("access = %q, calls = %d; want no refresh", tok.Access, is.calls.Load())
	}
}

func TestHasToken(t *testing.T) {
	st := newFakeStore()
	a := newClient(t, newIssuer(t), st)
	if a.HasToken(context.Background()) {
		t.Fatal("empty store must report no token")
	}
	st.set(KindRefreshToken, "r", nil)
	if !a.HasToken(context.Background()) {
		t.Fatal("refresh row must count as a credential")
	}
}

// --- loopback ---

func TestLoginFromLoopback_RefusesNonLoopbackBind(t *testing.T) {
	a := newClient(t, newIssuer(t), newFakeStore())
	if _, err := a.LoginFromLoopback(context.Background(), BrowserLogin{BindAddr: "0.0.0.0:0"}); err == nil {
		t.Fatal("a wildcard bind must be refused")
	}
}

func TestLoginFromLoopback_HappyPath(t *testing.T) {
	is := newIssuer(t)
	st := newFakeStore()
	a := newClient(t, is, st)
	var callbackErr error
	cfg := BrowserLogin{
		BindAddr: "127.0.0.1:0",
		OpenURL: func(authorizeURL string) error {
			u, err := url.Parse(authorizeURL)
			if err != nil {
				return err
			}
			redirect := u.Query().Get("redirect_uri")
			state := u.Query().Get("state")
			resp, err := http.Get(redirect + "?code=loop-code&state=" + url.QueryEscape(state))
			if err != nil {
				callbackErr = err
				return nil
			}
			_ = resp.Body.Close()
			return nil
		},
		Timeout: 5 * time.Second,
	}
	res, err := a.LoginFromLoopback(context.Background(), cfg)
	if err != nil {
		t.Fatalf("LoginFromLoopback: %v (callback: %v)", err, callbackErr)
	}
	if res.AccountID != "operator@example.com" {
		t.Errorf("AccountID = %q", res.AccountID)
	}
	if is.lastBody()["code"] != "loop-code" {
		t.Error("exchange did not carry the callback code")
	}
	// Redirect URI in the exchange matches the authorize URL exactly.
	if got := is.lastBody()["redirect_uri"]; !strings.HasPrefix(got.(string), "http://localhost:") {
		t.Errorf("redirect_uri = %v", got)
	}
}

// --- helpers ---

func stateOf(t *testing.T, authorizeURL string) string {
	t.Helper()
	u, err := url.Parse(authorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("state")
}

func readFixture(name string) ([]byte, error) {
	return os.ReadFile("../../../testdata/golden/anthropic-oauth/" + name) //nolint:gosec // fixed test fixture directory, name is a constant in the test
}
