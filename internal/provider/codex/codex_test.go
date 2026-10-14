package codex_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/provider/codex"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/openai"
	"github.com/tubeof/proxyllm/internal/translate/testutil"
)

// Codex provider contract tests, RED-GREEN.
// Expectations derive from the codex-rs sources (accessed 2026-09-11/12) —
// never from provider code behavior:
//   - the backend speaks the Responses-API wire shape at {base}/codex/responses
//     (provider-relative "/responses"); streams send `Accept: text/event-stream`.
//   - every backend request carries `originator: codex_cli_rs` and a User-Agent
//     `{originator}/{version} …` with PROxyLLM's OWN version constant (the CLI
//     pins a 0.0.0 build placeholder).
//   - `chatgpt-account-id` from the id_token's chatgpt_account_id JWT claim,
//     alongside Bearer access_token.
//   - OAI-Product-Sku is NO LONGER sent — must be absent.
//   - refresh is lazy on demand (expired/near-expiry access tokens are
//     refreshed before the request rides) with single-flight + atomic
//     persistence.
//   - error mapping, mid-stream terminal errors, cancel semantics (same
//     transport contract as the other HTTP providers).

const providerID = "codex-1"

// mustWrite/mustWriteString/drainEvents mirror the custom-provider test
// helpers (a handler write failure is always fatal; streams must not hang).
func mustWrite(t *testing.T, w io.Writer, data []byte) {
	t.Helper()
	if _, err := w.Write(data); err != nil {
		t.Fatalf("test handler write: %v", err)
	}
}

func mustWriteString(t *testing.T, w io.Writer, s string) {
	t.Helper()
	if _, err := io.WriteString(w, s); err != nil {
		t.Fatalf("test handler write: %v", err)
	}
}

func drainEvents(t *testing.T, events <-chan ir.StreamEvent) []ir.StreamEvent {
	t.Helper()
	var out []ir.StreamEvent
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-time.After(5 * time.Second):
			t.Fatal("timed out draining stream events")
		}
	}
}

func decodeSentBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("request body is not JSON: %v\n%s", err, raw)
	}
	return body
}

// makeJWT builds a structurally valid (unsigned) JWT whose payload carries
// the chatgpt_account_id claim. Test fixture only — never a credential.
func makeJWT(t *testing.T, accountID string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"chatgpt_account_id":%q}`, accountID)))
	return header + "." + payload + ".c2ln"
}

// memStore is an in-memory codex.TokenStore — the interface the SQLite store
// already satisfies (PutProviderTokens/GetProviderToken), so provider tests
// run without a database. Unlike a naive map it honors the ProviderID
// argument exactly like the production store, so a providerID mismatch
// between the auth client and the token rows SURFACES as ErrNotFound
// instead of being silently absorbed.
type memStore struct {
	mu     sync.Mutex
	tokens map[string]storage.ProviderToken // key: providerID + "\x00" + kind
}

func newMemStore() *memStore {
	return &memStore{tokens: map[string]storage.ProviderToken{}}
}

func memStoreKey(providerID, kind string) string { return providerID + "\x00" + kind }

func (m *memStore) PutProviderTokens(_ context.Context, toks ...storage.ProviderToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tok := range toks {
		if tok.ProviderID == "" || tok.Kind == "" {
			return errors.New("memstore: token without provider id or kind")
		}
		m.tokens[memStoreKey(tok.ProviderID, tok.Kind)] = tok
	}
	return nil
}

func (m *memStore) GetProviderToken(_ context.Context, providerID, kind string) (*storage.ProviderToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tok, ok := m.tokens[memStoreKey(providerID, kind)]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return &tok, nil
}

// get is the test-side reader for the seeded rows of THIS provider.
func (m *memStore) get(kind string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return string(m.tokens[memStoreKey(providerID, kind)].Secret)
}

// seedTokens plants the post-login token set: access token (expiry
// configurable for the lazy-refresh tests), id_token with the account claim,
// and a refresh token.
func seedTokens(t *testing.T, store codex.TokenStore, access string, expiresAt *int64) {
	t.Helper()
	err := store.PutProviderTokens(context.Background(),
		storage.ProviderToken{ProviderID: providerID, Kind: codex.KindAccessToken, Secret: []byte(access), ExpiresAt: expiresAt},
		storage.ProviderToken{ProviderID: providerID, Kind: codex.KindIDToken, Secret: []byte(makeJWT(t, "acct-123"))},
		storage.ProviderToken{ProviderID: providerID, Kind: codex.KindRefreshToken, Secret: []byte("r1")},
	)
	if err != nil {
		t.Fatalf("seed tokens: %v", err)
	}
}

func farFuture() *int64 {
	v := time.Now().Add(1 * time.Hour).Unix()
	return &v
}

func pastExpiry() *int64 {
	v := time.Now().Add(-1 * time.Minute).Unix()
	return &v
}

// newBackend wires the provider against one test server that plays both the
// ChatGPT backend ({base}/codex/responses) and the OAuth issuer
// ({issuer}/oauth/token) — paths keep them apart.
func newBackend(t *testing.T, refreshFn, responsesFn http.HandlerFunc) (*codex.Provider, *memStore, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", refreshFn)
	mux.HandleFunc("/codex/responses", responsesFn)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	store := newMemStore()
	seedTokens(t, store, "a1", farFuture())
	auth := codex.NewAuthClient(codex.AuthConfig{Issuer: srv.URL, HTTP: srv.Client(), Store: store, ProviderID: providerID})
	p := codex.New(codex.ProviderConfig{
		ID:           providerID,
		BaseURL:      srv.URL,
		Capabilities: provider.Capabilities{Streaming: true, Tools: true},
		Auth:         auth,
		HTTP:         srv.Client(),
	})
	return p, store, srv
}

// noopRefresh is the default /oauth/token handler: it counts hits so tests
// can assert NO refresh happened for fresh tokens.
func noopRefresh(t *testing.T, refreshHits *int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*refreshHits++
		w.WriteHeader(http.StatusOK)
		mustWriteString(t, w, `{"access_token":"a9","refresh_token":"r9"}`+"\n")
	}
}

func responsesRequest() ir.Request {
	return ir.Request{
		Model: "gpt-5",
		Messages: []ir.Message{{
			Role:  ir.RoleUser,
			Parts: []ir.Part{ir.TextPart{Text: "Tell me a joke."}},
		}},
	}
}

func TestProviderImplementsInterface(t *testing.T) {
	var _ provider.Provider = (*codex.Provider)(nil)
}

// TestComplete_RoundTrip: the request rides {base}/codex/responses
// in the Responses wire shape; the response decodes via the golden fixture.
// Headers: originator, own-version User-Agent, account id from
// the id_token claim + Bearer, and NO OAI-Product-Sku.
func TestComplete_RoundTrip(t *testing.T) {
	var refreshHits int32
	p, _, _ := newBackend(t, noopRefresh(t, &refreshHits), func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if want := "/codex/responses"; r.URL.Path != want {
			t.Errorf("path = %s, want %s", r.URL.Path, want)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer a1" {
			t.Errorf("Authorization = %q, want Bearer a1", got)
		}
		if got := r.Header.Get("Originator"); got != "codex_cli_rs" {
			t.Errorf("Originator = %q, want codex_cli_rs", got)
		}
		ua := r.Header.Get("User-Agent")
		if !strings.HasPrefix(ua, "codex_cli_rs/proxyllm/") {
			t.Errorf("User-Agent = %q, want the PROxyLLM-owned version constant prefix codex_cli_rs/proxyllm/", ua)
		}
		if got := r.Header.Get("Chatgpt-Account-Id"); got != "acct-123" {
			t.Errorf("Chatgpt-Account-Id = %q, want acct-123 from the id_token claim", got)
		}
		if got := r.Header.Get("OAI-Product-Sku"); got != "" {
			t.Errorf("OAI-Product-Sku = %q, want ABSENT", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q, want application/json for non-streaming", got)
		}
		body := decodeSentBody(t, r)
		if body["model"] != "gpt-5" {
			t.Errorf("request model = %v, want gpt-5", body["model"])
		}
		if stream, ok := body["stream"]; ok && stream != false {
			t.Errorf("request stream = %v, want false/absent for Complete", body["stream"])
		}
		w.Header().Set("Content-Type", "application/json")
		mustWrite(t, w, testutil.LoadGolden(t, "openai/responses-response.json"))
	})

	resp, err := p.Complete(context.Background(), responsesRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	want, err := openai.ResponsesCodec{}.DecodeResponse(testutil.LoadGolden(t, "openai/responses-response.json"))
	if err != nil {
		t.Fatalf("decode golden response: %v", err)
	}
	if !reflect.DeepEqual(resp, want) {
		t.Errorf("Complete response = %+v, want golden decode %+v", resp, want)
	}
	if refreshHits != 0 {
		t.Errorf("fresh token must not trigger a refresh, got %d /oauth/token hits", refreshHits)
	}
}

// TestComplete_LazyRefresh: an expired access token is refreshed BEFORE the
// request rides (lazy on-demand); the rotated pair is persisted and
// the backend sees the new Bearer.
func TestComplete_LazyRefresh(t *testing.T) {
	p, store, _ := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("refresh Content-Type = %q, want application/json", got)
		}
		body := decodeSentBody(t, r)
		if body["grant_type"] != "refresh_token" {
			t.Errorf("refresh grant_type = %v, want refresh_token", body["grant_type"])
		}
		if body["refresh_token"] != "r1" {
			t.Errorf("refresh_token = %v, want r1 (the stored one)", body["refresh_token"])
		}
		mustWriteString(t, w, `{"access_token":"a2","refresh_token":"r2"}`+"\n")
	}, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer a2" {
			t.Errorf("Authorization = %q, want Bearer a2 (refreshed before dispatch)", got)
		}
		w.Header().Set("Content-Type", "application/json")
		mustWrite(t, w, testutil.LoadGolden(t, "openai/responses-response.json"))
	})
	// Expire the seeded access token.
	seedTokens(t, store, "a1", pastExpiry())

	resp, err := p.Complete(context.Background(), responsesRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.ID == "" {
		t.Error("Complete returned an empty response")
	}
	if got := store.get(codex.KindAccessToken); got != "a2" {
		t.Errorf("access_token = %q, want a2 (rotation persisted)", got)
	}
	if got := store.get(codex.KindRefreshToken); got != "r2" {
		t.Errorf("refresh_token = %q, want r2 (single-use rotation persisted)", got)
	}
}

// TestComplete_UpstreamError: non-2xx maps to *provider.UpstreamError with
// both error tiers + Retry-After — same contract as every provider.
func TestComplete_UpstreamError(t *testing.T) {
	var refreshHits int32
	p, _, _ := newBackend(t, noopRefresh(t, &refreshHits), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		mustWrite(t, w, testutil.LoadGolden(t, "openai/error.json"))
	})

	_, err := p.Complete(context.Background(), responsesRequest())
	var up *provider.UpstreamError
	if !errors.As(err, &up) {
		t.Fatalf("Complete error = %v (%T), want *provider.UpstreamError", err, err)
	}
	if up.HTTPStatus != http.StatusTooManyRequests {
		t.Errorf("HTTPStatus = %d, want %d", up.HTTPStatus, http.StatusTooManyRequests)
	}
	if up.Code != "rate_limit_exceeded" {
		t.Errorf("Code = %q, want rate_limit_exceeded from the body", up.Code)
	}
	if up.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %v, want 7s", up.RetryAfter)
	}
}

// TestComplete_TransportError: a failure before any response is NOT an
// UpstreamError (routing's zero-status early-stream path).
func TestComplete_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()
	store := newMemStore()
	seedTokens(t, store, "a1", farFuture())
	p := codex.New(codex.ProviderConfig{
		ID:           providerID,
		BaseURL:      srv.URL,
		Capabilities: provider.Capabilities{},
		Auth:         codex.NewAuthClient(codex.AuthConfig{Issuer: srv.URL, HTTP: srv.Client(), Store: store, ProviderID: providerID}),
		HTTP:         srv.Client(),
	})

	_, err := p.Complete(context.Background(), responsesRequest())
	if err == nil {
		t.Fatal("Complete against a closed endpoint must fail")
	}
	var up *provider.UpstreamError
	if errors.As(err, &up) {
		t.Errorf("transport error must not be an UpstreamError, got %+v", up)
	}
}

// TestStream_Events: stream requests carry Accept: text/event-stream
// and stream:true; the golden SSE replays to the canonical event sequence.
func TestStream_Events(t *testing.T) {
	var refreshHits int32
	p, _, _ := newBackend(t, noopRefresh(t, &refreshHits), func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream for streams", got)
		}
		body := decodeSentBody(t, r)
		if body["stream"] != true {
			t.Errorf("request stream = %v, want true for stream requests", body["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		mustWrite(t, w, testutil.LoadGolden(t, "openai/responses-stream.sse"))
	})

	req := responsesRequest()
	req.Stream = true
	events, cancel, err := p.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer cancel()

	got := drainEvents(t, events)
	want := testutil.LoadExpectedEvents(t, "openai/responses-stream.expected.json")
	testutil.AssertEventsEqual(t, "openai/responses-stream.sse", want, got)
}

// TestStream_MidStreamFailure: an upstream break after the first event is a
// terminal ir.Error event — never a dispatch error.
func TestStream_MidStreamFailure(t *testing.T) {
	var refreshHits int32
	p, _, _ := newBackend(t, noopRefresh(t, &refreshHits), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		mustWriteString(t, w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_x\",\"model\":\"gpt-5\",\"status\":\"in_progress\"}}\n\n")
		flusher.Flush()
		mustWriteString(t, w, "data: {\"type\":\"resp") // truncated frame, then abrupt close
		hj := w.(http.Hijacker)
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		_ = conn.Close()
	})

	req := responsesRequest()
	req.Stream = true
	events, cancel, err := p.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream dispatch must succeed (the failure is mid-stream): %v", err)
	}
	defer cancel()

	got := drainEvents(t, events)
	if len(got) == 0 {
		t.Fatal("expected at least the start event before the failure")
	}
	last := got[len(got)-1]
	if last.Type != ir.Error {
		t.Fatalf("last event = %s, want a terminal ir.Error event", last.Type)
	}
	if last.Error == nil || last.Error.Message == "" {
		t.Error("terminal error event must carry the upstream failure message")
	}
}

// TestStream_Cancel: cancel aborts the upstream request and closes the
// channel promptly.
func TestStream_Cancel(t *testing.T) {
	var refreshHits int32
	p, _, _ := newBackend(t, noopRefresh(t, &refreshHits), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// Open the message item first — without an output_item.added the
		// decoder drops every delta (no open item), and no event would ever
		// reach the consumer.
		mustWriteString(t, w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"m\",\"role\":\"assistant\"}}\n\n")
		flusher.Flush()
		for i := 0; i < 1000; i++ {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			mustWriteString(t, w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"output_index\":0,\"content_index\":0,\"delta\":\"a\"}\n\n")
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	})

	req := responsesRequest()
	req.Stream = true
	events, cancel, err := p.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	select {
	case _, ok := <-events:
		if !ok {
			t.Fatal("channel closed before the first event")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no first event within 5s")
	}

	cancel()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return // closed — contract satisfied
			}
		case <-deadline:
			t.Fatal("cancel must close the event channel promptly")
		}
	}
}

// TestStream_DispatchFailure: a pre-stream HTTP error is returned as the
// Stream dispatch error, not delivered on the channel.
func TestStream_DispatchFailure(t *testing.T) {
	var refreshHits int32
	p, _, _ := newBackend(t, noopRefresh(t, &refreshHits), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		mustWrite(t, w, testutil.LoadGolden(t, "openai/error.json"))
	})

	req := responsesRequest()
	req.Stream = true
	events, _, err := p.Stream(context.Background(), req)
	if err == nil {
		for range events { // drain so a wrongly started pump exits
		}
		t.Fatal("Stream dispatch against a 500 must fail")
	}
	var up *provider.UpstreamError
	if !errors.As(err, &up) {
		t.Fatalf("dispatch error = %v (%T), want *provider.UpstreamError", err, err)
	}
	if up.HTTPStatus != http.StatusInternalServerError {
		t.Errorf("HTTPStatus = %d, want 500", up.HTTPStatus)
	}
}

// TestRefreshCredentials: the Provider interface method delegates to the
// auth client's rotation (single-flight + atomic persistence).
func TestRefreshCredentials(t *testing.T) {
	p, store, _ := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		mustWriteString(t, w, `{"access_token":"a3","refresh_token":"r3"}`+"\n")
	}, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no backend request expected in this test")
	})

	if err := p.RefreshCredentials(context.Background()); err != nil {
		t.Fatalf("RefreshCredentials: %v", err)
	}
	if got := store.get(codex.KindAccessToken); got != "a3" {
		t.Errorf("access_token = %q, want a3", got)
	}
	if got := store.get(codex.KindRefreshToken); got != "r3" {
		t.Errorf("refresh_token = %q, want r3", got)
	}
}
