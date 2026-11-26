package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// G4, local mirror of the M1 header cell: the client's credential stops at
// the proxy, the allow-listed headers reach the upstream, and hop-by-hop
// headers do not. The mock provider is the production custom-openai provider,
// so this runs without MockServer.
func TestHeaders_ClientCredentialNeverForwards(t *testing.T) {
	e := newEnv(t)
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))

	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/v1/chat/completions",
		strings.NewReader(string(golden(t, "openai/chat-request.json"))))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("x-api-key", e.key)
	req.Header.Set("x-stainless-lang", "go")
	req.Header.Set("Connection", "keep-alive, X-Test-Hop")
	req.Header.Set("X-Test-Hop", "drop-me")
	req.Header.Set("TE", "trailers")
	req.Header.Set("Upgrade", "websocket")
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	recs := e.mock.Requests()
	if len(recs) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(recs))
	}
	h := recs[0].Header
	if got := h.Get("Authorization"); got != "Bearer mock-key" {
		t.Errorf("upstream Authorization = %q, want the provider credential", got)
	}
	if got := h.Get("X-Api-Key"); got != "" {
		t.Errorf("client x-api-key reached the upstream: %q", got)
	}
	if got := h.Get("X-Stainless-Lang"); got != "go" {
		t.Errorf("allow-listed x-stainless-lang = %q, want forwarded", got)
	}
	for _, hop := range []string{"X-Test-Hop", "Te", "Upgrade"} {
		if got := h.Get(hop); got != "" {
			t.Errorf("hop-by-hop %s reached the upstream: %q", hop, got)
		}
	}
	if got := h.Get("Connection"); strings.Contains(got, "X-Test-Hop") {
		t.Errorf("Connection named header reached the upstream: %q", got)
	}
}
