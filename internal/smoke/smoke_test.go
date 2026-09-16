package smoke

// ENV-gated live smoke tests (Phase 1 DoD, Task #19; plan §8 step 7):
// PROXYLLM_SMOKE=1 opts in — they SKIP by default and NEVER run in the
// default CI suite. They pin the two research gaps the docs leave open:
//
//   - OP-3: the Codex refresh-grant body at {issuer}/oauth/token (X5) and
//     the ChatGPT backend request shape at {backend}/codex/responses (X7)
//     — the provider mirrors the authorization-code grant shape (X9);
//     the smoke verifies it against the LIVE issuer and reports the
//     observed shape so findings become pinned notes.
//   - OP-4: the Ollama native stream wire format — NDJSON lines with
//     done/done_reason (L6) — against a LIVE Ollama endpoint
//     (PROXYLLM_SMOKE_OLLAMA_URL, default the local
//     http://localhost:11434).
//
// Credentials come exclusively from ENV (never files, never config):
//   - OP-3 needs PROXYLLM_SMOKE_CODEX_REFRESH_TOKEN (a real refresh token)
//     and optionally PROXYLLM_SMOKE_CODEX_ISSUER (default
//     https://auth.openai.com) + PROXYLLM_SMOKE_CODEX_BACKEND (default
//     https://chatgpt.com/backend-api). The refresh token is used ONLY to
//     exercise the rotation; the smoke never logs it.
//   - OP-4 needs an Ollama instance; the local default needs no key.
//     PROXYLLM_SMOKE_OLLAMA_MODEL picks the model (default: first tag
//     from /api/tags), PROXYLLM_SMOKE_OLLAMA_KEY the bearer (L2).
//
// Smoke discipline: these tests NEVER assert on opaque token values and
// never echo raw upstream bodies; failures print status codes and decoded
// field names only (the §5.3 redaction doctrine applies to smokes too).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/config"
	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/provider/codex"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/ollama"
)

// smokeEnabled reports whether the operator opted in (§8: never default).
func smokeEnabled(t *testing.T) bool {
	t.Helper()
	if os.Getenv("PROXYLLM_SMOKE") != "1" {
		t.Skip("live smoke disabled: set PROXYLLM_SMOKE=1 (never enabled in default CI)")
		return false
	}
	return true
}

// smokeEnv reads one ENV var, skipping when unset. Empty value counts as
// unset — a smoke that needs a credential must not silently run without
// it.
func smokeEnv(t *testing.T, name, def string) string {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		if def == "" {
			t.Skipf("%s not set — set it to run this smoke", name)
		}
		return def
	}
	return v
}

// smokeHTTPClient is a short-leashed client: live endpoints answer fast
// or the smoke fails instead of hanging the suite.
func smokeHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// newSmokeStore opens the encrypted production store in a temp dir with a
// fixed test-only envelope key, so the token triple rides the exact
// production persistence path (OA-2).
func newSmokeStore(t *testing.T) *storage.Store {
	t.Helper()
	const keyHex = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	key, err := storage.LoadMasterKey(keyHex, true, "")
	if err != nil {
		t.Fatalf("smoke master key: %v", err)
	}
	envelope, err := storage.NewEnvelope(key)
	if err != nil {
		t.Fatalf("smoke envelope: %v", err)
	}
	st, err := storage.Open(config.StorageConfig{Path: t.TempDir() + "/smoke.db"})
	if err != nil {
		t.Fatalf("smoke store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	st.SetEnvelope(envelope)
	return st
}

// smokeChatRequest is the minimal request used for the OP-3 backend smoke
// (the provider's own encoder turns it into the production Responses
// shape, X7).
func smokeChatRequest() ir.Request {
	return ir.Request{
		Model:     "gpt-5",
		MaxTokens: 64,
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "Reply with exactly: hello"}}},
		},
	}
}

// ---------------------------------------------------------------------------
// OP-3: Codex refresh grant + ChatGPT backend request shape
// ---------------------------------------------------------------------------

// newSmokeCodexAuth seeds the token store exactly like a completed browser
// login — only the refresh token is present; access/id tokens arrive on
// rotation — and builds the OAuth client against the live issuer.
func newSmokeCodexAuth(t *testing.T) codex.Auth {
	t.Helper()
	refreshToken := smokeEnv(t, "PROXYLLM_SMOKE_CODEX_REFRESH_TOKEN", "")
	issuer := smokeEnv(t, "PROXYLLM_SMOKE_CODEX_ISSUER", "https://auth.openai.com")

	st := newSmokeStore(t)
	if err := st.PutProviderTokens(context.Background(), storage.ProviderToken{
		ProviderID: "smoke-codex",
		Kind:       codex.KindRefreshToken,
		Secret:     []byte(refreshToken),
	}); err != nil {
		t.Fatalf("seed refresh token: %v", err)
	}
	return codex.NewAuthClient(codex.AuthConfig{
		Issuer:     issuer,
		HTTP:       smokeHTTPClient(),
		Store:      st,
		ProviderID: "smoke-codex",
	})
}

// TestSmoke_CodexRefreshGrant verifies the OP-3 refresh-grant hypothesis
// (mirror the authorization-code grant at {issuer}/oauth/token, X9)
// against the LIVE issuer. A failure here means the documented hypothesis
// is wrong — the finding must be escalated (plan §2.9), not worked around
// in the test.
func TestSmoke_CodexRefreshGrant(t *testing.T) {
	if !smokeEnabled(t) {
		return
	}
	auth := newSmokeCodexAuth(t)
	ctx := context.Background()

	// The hypothesis under test: the mirrored grant rotates the pair.
	if err := auth.Refresh(ctx); err != nil {
		t.Fatalf("OP-3 refresh grant rejected by live issuer: %v\n"+
			"the mirrored grant shape (X9) does not match the live issuer — escalate per plan §2.9, do not adapt the test",
			err)
	}

	// Rotation must have persisted a fresh access token (X18 lazy read
	// path now short-circuits on a valid, un-expired token).
	tok, err := auth.AccessToken(ctx)
	if err != nil {
		t.Fatalf("access token after rotation: %v", err)
	}
	if tok.Access == "" {
		t.Fatal("access token empty after successful rotation")
	}
	if tok.AccountID == "" {
		t.Error("account id empty — the id_token chatgpt_account_id claim (X15) was not extractable")
	}
}

// TestSmoke_CodexBackendShape verifies the OP-3 backend request shape: a
// rotated access token performs a minimal request against the LIVE ChatGPT
// backend and gets a protocol answer (never a transport failure or silent
// hang) — pinning that the backend accepts the mirror headers
// (Originator/User-Agent/Chatgpt-Account-Id, X14/X15/X24).
func TestSmoke_CodexBackendShape(t *testing.T) {
	if !smokeEnabled(t) {
		return
	}
	backend := smokeEnv(t, "PROXYLLM_SMOKE_CODEX_BACKEND", "https://chatgpt.com/backend-api")
	auth := newSmokeCodexAuth(t)
	ctx := context.Background()

	if err := auth.Refresh(ctx); err != nil {
		t.Fatalf("prerequisite rotation failed: %v", err)
	}

	p := codex.New(codex.ProviderConfig{
		ID:      "smoke-codex",
		BaseURL: backend,
		Capabilities: provider.Capabilities{
			Streaming: true, Tools: true, Vision: true,
			WebSearch: true, WebFetch: true, CodeExecution: true, Thinking: true,
		},
		Auth: auth,
		HTTP: smokeHTTPClient(),
	})

	resp, err := p.Complete(ctx, smokeChatRequest())
	if err != nil {
		// Distinguish the two failure families (OP-3 escalation needs the
		// right hypothesis to falsify): a live backend ANSWERING with an
		// unexpected shape (UpstreamError with an HTTP status) falsifies the
		// mirrored request shape (X7/X14/X15/X24); an error WITHOUT one is a
		// transport/network failure (offline, DNS, TLS) and says nothing
		// about the shape hypothesis.
		var upErr *provider.UpstreamError
		if errors.As(err, &upErr) && upErr.HTTPStatus > 0 {
			t.Fatalf("OP-3 backend answered HTTP %d — the mirrored backend shape does not match the live backend; escalate per plan §2.9", upErr.HTTPStatus)
		}
		t.Fatalf("OP-3 backend request failed before any HTTP answer (transport failure or credential rotation failure): %v\n"+
			"this is not a shape mismatch — check reachability and the refresh token before escalating the shape hypothesis", err)
	}
	if len(resp.Content) == 0 {
		t.Error("backend answer carries no content parts")
	}
}

// ---------------------------------------------------------------------------
// OP-4: Ollama native stream wire format
// ---------------------------------------------------------------------------

// TestSmoke_OllamaNativeStream pins the native /api/chat NDJSON wire
// format (L6): every line a standalone JSON object, the final line
// carrying done:true + done_reason + full usage counts. The smoke records
// the observed done_reason value so the residual risk (unknown value
// strings) is pinned by observation, not assumption.
func TestSmoke_OllamaNativeStream(t *testing.T) {
	if !smokeEnabled(t) {
		return
	}
	base := smokeEnv(t, "PROXYLLM_SMOKE_OLLAMA_URL", "http://localhost:11434")
	model := os.Getenv("PROXYLLM_SMOKE_OLLAMA_MODEL")
	if model == "" {
		model = firstOllamaTag(t, base)
	}

	reqBody, err := ollama.Codec{}.EncodeRequest(ir.Request{
		Model:     model,
		MaxTokens: 64,
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "Reply with exactly: hello"}}},
		},
		Stream: true,
	})
	if err != nil {
		t.Fatalf("encode /api/chat request: %v", err)
	}

	httpReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost, base+"/api/chat", strings.NewReader(string(reqBody)))
	if err != nil {
		t.Fatalf("build /api/chat request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if key := os.Getenv("PROXYLLM_SMOKE_OLLAMA_KEY"); key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := smokeHTTPClient().Do(httpReq)
	if err != nil {
		t.Fatalf("live /api/chat POST: %v (is Ollama running at %s?)", err, base)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("live /api/chat status %d — expected 200 (model %q available?)", resp.StatusCode, model)
	}

	// Decode through the production NDJSON stream machine and pin the wire
	// facts by observation: terminal done:true + done_reason + usage.
	dec := ollama.NewStreamDecoder(resp.Body)
	var sawTerminal bool
	var doneReason string
	var finalUsage ir.Usage
	for {
		ev, err := dec.Read(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NDJSON stream decode: %v — the pinned wire assumptions (L6/L10) do not match the live stream; escalate per plan §2.9", err)
		}
		if ev.Type == ir.MessageDelta && ev.MessageDelta != nil {
			sawTerminal = true
			doneReason = string(ev.MessageDelta.StopReason)
			finalUsage = ev.MessageDelta.Usage
		}
	}
	if !sawTerminal {
		t.Fatal("stream ended without a terminal message_delta (done:true frame)")
	}
	t.Logf("OP-4 pinned: done_reason=%q usage input/output=%d/%d", doneReason, finalUsage.InputTokens, finalUsage.OutputTokens)
}

// firstOllamaTag picks the first model tag from /api/tags (L3) so the
// smoke runs against whatever the local instance hosts.
func firstOllamaTag(t *testing.T, base string) string {
	t.Helper()
	resp, err := smokeHTTPClient().Get(base + "/api/tags")
	if err != nil {
		t.Skipf("no Ollama at %s (%v) — set PROXYLLM_SMOKE_OLLAMA_URL to a live instance", base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("/api/tags status %d at %s — instance not usable", resp.StatusCode, base)
	}
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tags); err != nil {
		t.Skipf("/api/tags decode: %v", err)
	}
	if len(tags.Models) == 0 {
		t.Skip("no models hosted at the smoke instance — pull one or set PROXYLLM_SMOKE_OLLAMA_MODEL")
	}
	return tags.Models[0].Name
}
