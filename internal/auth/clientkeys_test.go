package auth

// Client-key auth: RED tests covering key generation and the middleware
// surface (auth extraction per
// surface, both header families being real in the wild,
// Argon2id-only hashing,
// display-once secrets, revocation immediate, protocol-conform 401
// bodies per surface, and the client_keys schema).
// Never from implementation behavior.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/config"
	"github.com/tubeof/proxyllm/internal/storage"
)

// newTestStore opens a store over a fresh temp-dir database, closed at
// cleanup (storage suite convention).
func newTestStore(t *testing.T) *storage.Store {
	t.Helper()
	st, err := storage.Open(config.StorageConfig{Path: t.TempDir() + "/test.db"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// serveAuth starts the middleware under test around an OK handler.
func serveAuth(t *testing.T, mw *Middleware) *httptest.Server {
	t.Helper()
	handler := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ClientKeyIDFromContext(r.Context()) == "" {
			t.Error("authenticated request carries no client key id in context")
		}
		w.WriteHeader(http.StatusOK)
	}))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// do sends one request with optional headers and returns status + body.
func do(t *testing.T, url string, hdr string, value string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hdr != "" {
		req.Header.Set(hdr, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// 1. Key shape: pk_<prefix>_<secret> with a 256-bit secret
// (43 unpadded base64url chars); two generations never collide.
func TestGenerateKey_Shape(t *testing.T) {
	k1, err := GenerateClientKey()
	if err != nil {
		t.Fatalf("GenerateClientKey: %v", err)
	}
	if !strings.HasPrefix(k1, "pk_") {
		t.Fatalf("key %q must start with pk_", k1)
	}
	parts := strings.SplitN(k1, "_", 3)
	if len(parts) != 3 {
		t.Fatalf("key %q must be pk_<prefix>_<secret>", k1)
	}
	if len(parts[2]) != 43 {
		t.Errorf("secret length = %d, want 43 base64url chars (256 bit)", len(parts[2]))
	}
	if len(parts[1]) < 4 {
		t.Errorf("prefix %q too short for the display hint", parts[1])
	}

	k2, err := GenerateClientKey()
	if err != nil {
		t.Fatalf("GenerateClientKey: %v", err)
	}
	if k1 == k2 {
		t.Error("two generated keys are identical — entropy failure")
	}
}

// 2. Create + verify round trip (G control 1): the stored row holds only
// the Argon2id PHC hash + display prefix — never the secret; verification
// accepts the original key and rejects everything else.
func TestCreateAndVerify_RoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, secret, err := CreateClientKey(ctx, st, "test-key")
	if err != nil {
		t.Fatalf("CreateClientKey: %v", err)
	}
	if id == "" || secret == "" {
		t.Fatal("empty id or secret from CreateClientKey")
	}

	// Display-once discipline: the database must not contain the secret.
	row, err := st.GetClientKey(ctx, id)
	if err != nil {
		t.Fatalf("GetClientKey: %v", err)
	}
	if strings.Contains(row.KeyHash, secret) {
		t.Error("DB row contains the plaintext secret — display-once violated")
	}
	if !strings.HasPrefix(row.KeyHash, "$argon2id$") {
		t.Errorf("key_hash = %q, want an Argon2id PHC string", row.KeyHash)
	}
	if !strings.HasPrefix(secret, "pk_"+row.KeyPrefix+"_") {
		t.Errorf("secret %q does not match stored prefix %q", secret, row.KeyPrefix)
	}
	if row.Revoked {
		t.Error("fresh key is revoked")
	}

	if !VerifyClientKey(ctx, st, secret) {
		t.Error("VerifyClientKey rejected the original secret")
	}
	if VerifyClientKey(ctx, st, "pk_"+row.KeyPrefix+"_wrong") {
		t.Error("VerifyClientKey accepted a wrong secret with valid prefix")
	}
}

// 3. Multiple keys coexist (WebUI /keys list); each verifies only against
// its own secret; list returns all with names.
func TestMultipleKeys_ListAndVerify(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	_, s1, err := CreateClientKey(ctx, st, "alpha")
	if err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	_, s2, err := CreateClientKey(ctx, st, "beta")
	if err != nil {
		t.Fatalf("create beta: %v", err)
	}
	if !VerifyClientKey(ctx, st, s1) || !VerifyClientKey(ctx, st, s2) {
		t.Error("multi-key verification failed")
	}
	if VerifyClientKey(ctx, st, s1[:len(s1)-1]+"x") {
		t.Error("tampered key verified")
	}

	keys, err := st.ListClientKeys(ctx)
	if err != nil {
		t.Fatalf("ListClientKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("listed keys = %d, want 2", len(keys))
	}
	names := map[string]bool{keys[0].Name: true, keys[1].Name: true}
	if !names["alpha"] || !names["beta"] {
		t.Errorf("listed key names = %v, want alpha and beta", keys[0].Name+", "+keys[1].Name)
	}
}

// 4. Scopes round trip (the scopes column): stored scopes survive
// create and retrieval (routing rule 1 consumes them later).
func TestScopes_RoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, _, err := CreateClientKeyWithScopes(ctx, st, "scoped",
		json.RawMessage(`["claude-*","gpt-*"]`))
	if err != nil {
		t.Fatalf("CreateClientKeyWithScopes: %v", err)
	}
	row, err := st.GetClientKey(ctx, id)
	if err != nil {
		t.Fatalf("GetClientKey: %v", err)
	}
	var scopes []string
	if err := json.Unmarshal(row.Scopes, &scopes); err != nil {
		t.Fatalf("scopes not JSON: %v (%s)", err, row.Scopes)
	}
	if len(scopes) != 2 || scopes[0] != "claude-*" || scopes[1] != "gpt-*" {
		t.Errorf("scopes = %v, want [claude-* gpt-*]", scopes)
	}
}

// 5. Revocation (G control 1): a revoked key must not verify, even with
// the correct secret.
func TestRevoke(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, secret, err := CreateClientKey(ctx, st, "rev-me")
	if err != nil {
		t.Fatalf("CreateClientKey: %v", err)
	}
	if !VerifyClientKey(ctx, st, secret) {
		t.Fatal("pre-revoke verification sanity failed")
	}

	if err := st.RevokeClientKey(ctx, id); err != nil {
		t.Fatalf("RevokeClientKey: %v", err)
	}
	if VerifyClientKey(ctx, st, secret) {
		t.Error("revoked key still verifies")
	}
}

// 6. Middleware header extraction: Bearer on the OpenAI surface,
// x-api-key on the Anthropic surface, and — because
// both header families are real in the wild — Bearer on the Anthropic
// surface too. Missing/wrong/malformed credentials → 401.
func TestMiddleware_AuthExtraction(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	_, secret, err := CreateClientKey(ctx, st, "mw-test")
	if err != nil {
		t.Fatalf("CreateClientKey: %v", err)
	}

	mw := NewMiddleware(st)
	srv := serveAuth(t, mw)

	cases := []struct {
		name   string
		header string
		value  string
		want   int
	}{
		{"Bearer", "Authorization", "Bearer " + secret, http.StatusOK},
		{"x-api-key", "x-api-key", secret, http.StatusOK},
		{"Bearer recognized on both surfaces", "Authorization", "Bearer " + secret, http.StatusOK},
		{"no header → 401", "", "", http.StatusUnauthorized},
		{"wrong secret → 401", "Authorization", "Bearer pk_deadbeef_deadbeefdeadbeefdeadbeefdeadbeefdeadbeefde", http.StatusUnauthorized},
		{"malformed Bearer (no space) → 401", "Authorization", "Bearer" + secret, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := do(t, srv.URL+"/v1/messages", tc.header, tc.value)
			if got != tc.want {
				t.Errorf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

// 7. Protocol-conform 401 bodies per surface: OpenAI
// error.type/code/message on /v1/chat/completions; Anthropic
// error.type/message on /v1/messages.
func TestMiddleware_401BodyPerSurface(t *testing.T) {
	st := newTestStore(t)
	mw := NewMiddleware(st)
	handler := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unauthenticated request reached the handler")
	}))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// OpenAI surface shape.
	status, body := do(t, srv.URL+"/v1/chat/completions", "Authorization", "Bearer pk_no_no_no")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	var openaiErr struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &openaiErr); err != nil {
		t.Fatalf("openai 401 body not JSON: %v (%s)", err, body)
	}
	if openaiErr.Error.Message == "" || openaiErr.Error.Type == "" {
		t.Errorf("openai 401 body missing type/message: %s", body)
	}

	// Anthropic surface shape: {"type":"error","error":{...}}.
	status, body = do(t, srv.URL+"/v1/messages", "x-api-key", "pk_no_no_no")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	var anthErr struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &anthErr); err != nil {
		t.Fatalf("anthropic 401 body not JSON: %v (%s)", err, body)
	}
	if anthErr.Type != "error" || anthErr.Error.Type == "" || anthErr.Error.Message == "" {
		t.Errorf("anthropic 401 body = %s, want {type:error, error:{type,message}}", body)
	}
}

// 8. Immediate revocation through the middleware: a key that authenticated
// successfully must stop working as soon as it is revoked — no TTL window
// (G control 1; /keys revoke must take effect at once).
func TestMiddleware_RevokedKeyStopsWorkingImmediately(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id, secret, err := CreateClientKey(ctx, st, "rev-mw")
	if err != nil {
		t.Fatalf("CreateClientKey: %v", err)
	}

	mw := NewMiddleware(st)
	srv := serveAuth(t, mw)

	if got, _ := do(t, srv.URL+"/v1/messages", "Authorization", "Bearer "+secret); got != http.StatusOK {
		t.Fatalf("pre-revoke status = %d, want 200", got)
	}
	if err := st.RevokeClientKey(ctx, id); err != nil {
		t.Fatalf("RevokeClientKey: %v", err)
	}
	if got, _ := do(t, srv.URL+"/v1/messages", "Authorization", "Bearer "+secret); got != http.StatusUnauthorized {
		t.Errorf("post-revoke status = %d, want 401 — revocation must be immediate", got)
	}
}

// 9. Auth-disabled opt-out (G control 1: PROXYLLM_AUTH_DISABLED=1 bypasses
// with a loud startup warning — the option mirrors that ENV switch). The
// request still reaches the handler; no key id in the context.
func TestMiddleware_Disabled(t *testing.T) {
	st := newTestStore(t)
	mw := NewMiddleware(st, WithAuthDisabled())
	handler := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ClientKeyIDFromContext(r.Context()) != "" {
			t.Error("anonymous context carries a key id")
		}
		w.WriteHeader(http.StatusOK)
	}))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	got, _ := do(t, srv.URL+"/v1/messages", "", "")
	if got != http.StatusOK {
		t.Errorf("auth-disabled middleware rejected request: %d", got)
	}
}

// 10. last_used_at (a schema field): a successful verification
// stamps it, so the WebUI can show staleness.
func TestLastUsed_UpdatedOnSuccessfulAuth(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id, secret, err := CreateClientKey(ctx, st, "used")
	if err != nil {
		t.Fatalf("CreateClientKey: %v", err)
	}

	row, err := st.GetClientKey(ctx, id)
	if err != nil {
		t.Fatalf("GetClientKey: %v", err)
	}
	if row.LastUsedAt != nil {
		t.Fatalf("fresh key last_used_at = %v, want nil", *row.LastUsedAt)
	}

	if !VerifyClientKey(ctx, st, secret) {
		t.Fatal("verification failed")
	}
	row2, err := st.GetClientKey(ctx, id)
	if err != nil {
		t.Fatalf("GetClientKey: %v", err)
	}
	if row2.LastUsedAt == nil {
		t.Fatal("last_used_at not stamped after successful verification")
	}
	if *row2.LastUsedAt < row.CreatedAt {
		t.Errorf("last_used_at %d before created_at %d", *row2.LastUsedAt, row.CreatedAt)
	}
}
