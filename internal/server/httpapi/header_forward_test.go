package httpapi

// Header forwarding: allow-listed Anthropic/OpenAI surface headers reach the
// upstream; credentials and cookies never do.

import (
	"net/http"
	"strings"
	"testing"
)

func TestMessages_ForwardsAllowListedHeaders(t *testing.T) {
	e := newClaudeEnv(t)
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "anthropic/message-response.json"))))

	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/v1/messages",
		strings.NewReader(string(golden(t, "anthropic/message-request.json"))))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("anthropic-beta", "test-beta-1")
	req.Header.Set("anthropic-version", "2024-06-01")
	req.Header.Set("x-stainless-os", "Linux")
	req.Header.Set("Cookie", "session=secret")

	status, body := e.do(t, req)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}
	rec := e.mock.Requests()
	if len(rec) == 0 {
		t.Fatal("upstream saw no request")
	}
	if got := rec[0].Header.Get("anthropic-beta"); got != "test-beta-1" {
		t.Errorf("anthropic-beta = %q, want forwarded", got)
	}
	if got := rec[0].Header.Get("x-stainless-os"); got != "Linux" {
		t.Errorf("x-stainless-os = %q, want forwarded", got)
	}
	if got := rec[0].Header.Values("anthropic-version"); len(got) != 1 || got[0] != "2024-06-01" {
		t.Errorf("anthropic-version = %v, want exactly the client value", got)
	}
	if got := rec[0].Header.Get("Cookie"); got != "" {
		t.Errorf("Cookie leaked upstream: %q", got)
	}
	if got := rec[0].Header.Get("Authorization"); got != "Bearer mock-key" {
		t.Errorf("Authorization = %q, want the provider key, never the client key", got)
	}
}
