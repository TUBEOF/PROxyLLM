package fakeidp_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/provider/anthropicoauth"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/testutil/fakeidp"
)

type memStore struct {
	mu   sync.Mutex
	toks map[string]storage.ProviderToken
}

func newMemStore() *memStore { return &memStore{toks: map[string]storage.ProviderToken{}} }

func (s *memStore) PutProviderTokens(_ context.Context, toks ...storage.ProviderToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range toks {
		s.toks[t.ProviderID+"/"+t.Kind] = t
	}
	return nil
}

func (s *memStore) GetProviderToken(_ context.Context, pid, kind string) (*storage.ProviderToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.toks[pid+"/"+kind]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return &t, nil
}

func (s *memStore) set(pid, kind, secret string, expires *int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toks[pid+"/"+kind] = storage.ProviderToken{ProviderID: pid, Kind: kind, Secret: []byte(secret), ExpiresAt: expires}
}

func TestPasteLoginAndRefreshRotation(t *testing.T) {
	idp := fakeidp.New(anthropicoauth.OAuthClientID)
	defer idp.Close()
	st := newMemStore()
	client := anthropicoauth.NewAuthClient(anthropicoauth.AuthConfig{
		AuthorizeURL: idp.URL + "/authorize",
		TokenURL:     idp.URL + "/token",
		Store:        st,
		ProviderID:   "acc-1",
	})

	flow, err := client.StartPasteLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(flow.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("client_id") != anthropicoauth.OAuthClientID {
		t.Fatalf("authorize client_id = %q", u.Query().Get("client_id"))
	}
	callback := idp.Approve(u.Query().Get("code_challenge"), flow.RedirectURI, u.Query().Get("state"))
	if !strings.HasPrefix(callback, flow.RedirectURI+"?") {
		t.Fatalf("callback = %s", callback)
	}
	res, err := client.CompletePasteLogin(context.Background(), flow.ID, callback)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if res.AccountID != "operator@example.com" {
		t.Errorf("account = %q", res.AccountID)
	}

	// Force a rotation: a fresh client (no cadence stamp) sees the expired
	// access token and rotates the single-use refresh token.
	expired := time.Now().Add(-time.Hour).Unix()
	st.set("acc-1", anthropicoauth.KindAccessToken, "stale", &expired)
	fresh := anthropicoauth.NewAuthClient(anthropicoauth.AuthConfig{
		AuthorizeURL: idp.URL + "/authorize",
		TokenURL:     idp.URL + "/token",
		Store:        st,
		ProviderID:   "acc-1",
	})
	tok, err := fresh.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("access token: %v", err)
	}
	if !strings.HasPrefix(tok.Access, "access-") {
		t.Errorf("access = %q, want a minted token", tok.Access)
	}
	if idp.Rotations() != 1 {
		t.Errorf("issuer rotations = %d, want 1", idp.Rotations())
	}
}

func TestRefreshWithRevokedTokenRequiresRelogin(t *testing.T) {
	idp := fakeidp.New(anthropicoauth.OAuthClientID)
	defer idp.Close()
	st := newMemStore()
	st.set("acc-2", anthropicoauth.KindRefreshToken, "revoked", nil)
	client := anthropicoauth.NewAuthClient(anthropicoauth.AuthConfig{
		AuthorizeURL: idp.URL + "/authorize",
		TokenURL:     idp.URL + "/token",
		Store:        st,
		ProviderID:   "acc-2",
	})
	err := client.Refresh(context.Background())
	var rl *anthropicoauth.ReLoginRequiredError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want ReLoginRequiredError", err)
	}
}
