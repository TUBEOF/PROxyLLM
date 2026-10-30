package codex_test

// HasCredential runs on every routing candidate for every request; it must be
// a cached answer, never a store read or an issuer refresh.

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/provider/codex"
)

func TestProvider_HasCredentialDoesNotRefresh(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "unexpected issuer call", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	store := freshStore()
	if err := seedRefreshable(store, "r1"); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	auth := testAuth(t, srv.URL, store)
	p := codex.New(codex.ProviderConfig{
		ID:           "p",
		BaseURL:      srv.URL,
		Auth:         auth,
		Capabilities: provider.Capabilities{},
	})
	if !p.HasCredential() {
		t.Fatal("HasCredential = false, want true")
	}
	if hits.Load() != 0 {
		t.Fatalf("HasCredential hit the issuer %d time(s)", hits.Load())
	}
}
