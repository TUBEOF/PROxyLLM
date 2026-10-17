package codex_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/provider/codex"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/translate/ir"
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

// semverRe is MAJOR.MINOR.PATCH — the shape codex-rs announces as
// client_version (models-manager client_version_to_whole).
var semverRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

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
	_, body := captureSentBody(t, r)
	return body
}

// captureSentBody reads the request body ONCE and returns it both raw and
// decoded: the round-trip test asserts on the exact bytes (the preview must
// equal what went out) and on the decoded fields.
func captureSentBody(t *testing.T, r *http.Request) ([]byte, map[string]any) {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("request body is not JSON: %v\n%s", err, raw)
	}
	return raw, body
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

// foldEvents assembles a canonical event sequence the way a non-streaming
// caller reads it: text/thinking deltas and tool-input fragments concatenate
// onto their open block, stop reason and usage ride the terminal
// MessageDelta. Written independently of the provider's assembler so the two
// must agree.
func foldEvents(t *testing.T, events []ir.StreamEvent) ir.Response {
	t.Helper()
	var (
		resp  ir.Response
		parts []ir.Part
		args  []string
	)
	for _, ev := range events {
		switch ev.Type {
		case ir.Start:
			resp.ID, resp.Model = ev.Response.ID, ev.Response.Model
		case ir.BlockStart:
			parts = append(parts, ev.Part)
			args = append(args, "")
		case ir.Delta:
			if ev.Index < 0 || ev.Index >= len(parts) {
				t.Fatalf("delta for unopened block index %d", ev.Index)
			}
			switch ev.Delta.Kind {
			case ir.DeltaText:
				p := parts[ev.Index].(ir.TextPart)
				p.Text += ev.Delta.Text
				parts[ev.Index] = p
			case ir.DeltaThinking:
				p := parts[ev.Index].(ir.ThinkingPart)
				p.Thinking += ev.Delta.Text
				parts[ev.Index] = p
			case ir.DeltaToolInput:
				args[ev.Index] += ev.Delta.PartialJSON
			}
		case ir.MessageDelta:
			resp.StopReason = ev.MessageDelta.StopReason
			resp.Usage = ev.MessageDelta.Usage
		}
	}
	for i, part := range parts {
		if tc, ok := part.(ir.ToolCallPart); ok && args[i] != "" {
			tc.Input = json.RawMessage(args[i])
			parts[i] = tc
		}
	}
	resp.Content = parts
	return resp
}

// TestComplete_RoundTrip: the ChatGPT backend has no non-streaming mode — the
// codex CLI's ResponsesApiRequest always sets stream:true, and a body without
// it is answered 400 {"detail":"Stream must be set to true"} (observed live
// 2026-10-17; fixture openai-codex/responses-request.json). Complete
// therefore dispatches the streamed round trip — Accept: text/event-stream,
// stream:true, store:false — and assembles the canonical events into one
// response. Headers: originator, own-version User-Agent, account id from
// the id_token claim + Bearer, and NO OAI-Product-Sku.
func TestComplete_RoundTrip(t *testing.T) {
	var refreshHits int32
	var sentBody []byte
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
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream — the backend only answers a streamed request", got)
		}
		var body map[string]any
		sentBody, body = captureSentBody(t, r)
		if body["model"] != "gpt-5" {
			t.Errorf("request model = %v, want gpt-5", body["model"])
		}
		if stream, ok := body["stream"]; !ok || stream != true {
			t.Errorf("request stream = %v (present=%v), want true — the backend rejects a body without it", stream, ok)
		}
		if store, ok := body["store"]; !ok || store != false {
			t.Errorf("request store = %v (present=%v), want false — the backend rejects a body without it", store, ok)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		mustWrite(t, w, testutil.LoadGolden(t, "openai/responses-stream.sse"))
	})

	resp, err := p.Complete(context.Background(), responsesRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	want := foldEvents(t, testutil.LoadExpectedEvents(t, "openai/responses-stream.expected.json"))
	if !reflect.DeepEqual(resp, want) {
		t.Errorf("Complete response = %+v, want the fixture events folded to %+v", resp, want)
	}
	// The fixture's own values, spelled out: the fold above must not be able
	// to agree with the assembler by accident.
	if resp.ID != "resp_67c9fdcecf488190bdd9a0409de3a1ec07b8b0ad4e5eb654" || resp.Model != "gpt-6-astra" {
		t.Errorf("id/model = %q/%q, want the fixture's resp_67c9…/gpt-6-astra", resp.ID, resp.Model)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("content = %+v, want one text part", resp.Content)
	}
	if tp, ok := resp.Content[0].(ir.TextPart); !ok || tp.Text != "Hi there! How can I assist you today?" {
		t.Errorf("content[0] = %+v, want the fixture's text part", resp.Content[0])
	}
	if resp.StopReason != ir.StopEndTurn {
		t.Errorf("stop reason = %q, want end_turn", resp.StopReason)
	}
	if resp.Usage.InputTokens != 37 || resp.Usage.OutputTokens != 11 {
		t.Errorf("usage = %+v, want the fixture's 37/11", resp.Usage)
	}

	// The preview must be the body that actually went out — including the
	// stream flag Complete forces.
	prev, err := p.PreviewRequest(responsesRequest())
	if err != nil {
		t.Fatalf("PreviewRequest: %v", err)
	}
	if !bytes.Equal(prev, sentBody) {
		t.Errorf("PreviewRequest = %s, want the sent body %s", prev, sentBody)
	}
	if refreshHits != 0 {
		t.Errorf("fresh token must not trigger a refresh, got %d /oauth/token hits", refreshHits)
	}
}

// The request fixture pins the backend's mandatory stream flag, so the
// round-trip assertion derives from the fixture, not from the encoder.
func TestCodexRequestFixture_StreamIsTrue(t *testing.T) {
	var fixture struct {
		Stream *bool `json:"stream"`
	}
	if err := json.Unmarshal(testutil.LoadGolden(t, "openai-codex/responses-request.json"), &fixture); err != nil {
		t.Fatalf("decode golden fixture: %v", err)
	}
	if fixture.Stream == nil || !*fixture.Stream {
		t.Errorf("golden fixture must pin stream=true, got %v", fixture.Stream)
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
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream (every backend request is streamed)", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		mustWrite(t, w, testutil.LoadGolden(t, "openai/responses-stream.sse"))
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

// A stream that breaks before its terminal event fails Complete: the
// non-streaming caller holds nothing yet, so the break is reported as an
// error — with status 0, since no HTTP answer described it.
func TestComplete_MidStreamFailure(t *testing.T) {
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

	_, err := p.Complete(context.Background(), responsesRequest())
	if err == nil {
		t.Fatal("Complete must fail when the stream breaks before its terminal event")
	}
	var up *provider.UpstreamError
	if !errors.As(err, &up) {
		t.Fatalf("Complete error = %v (%T), want *provider.UpstreamError", err, err)
	}
	if up.HTTPStatus != 0 {
		t.Errorf("HTTPStatus = %d, want 0 — mid-stream failures carry no HTTP answer", up.HTTPStatus)
	}
	if up.Message == "" {
		t.Error("the failure must carry the upstream's own message")
	}
}

// The caller's cancel is not an observation about the account: Complete
// reports the context error itself, which routing classifies as a client
// abort — never as a reason to cool the codex account down.
func TestComplete_Cancel(t *testing.T) {
	var refreshHits int32
	streaming := make(chan struct{})
	p, _, _ := newBackend(t, noopRefresh(t, &refreshHits), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// Open a message item so the consumer sees the stream start before
		// the cancel lands.
		mustWriteString(t, w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"m\",\"role\":\"assistant\"}}\n\n")
		flusher.Flush()
		close(streaming)
		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-streaming
		cancel()
	}()

	_, err := p.Complete(ctx, responsesRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Complete error = %v, want context.Canceled (the caller walked away)", err)
	}
}

// Streamed tool calls and reasoning assemble into one non-streaming response:
// the function_call item's {call_id, name} open the tool block, the argument
// fragments concatenate into its complete JSON input, and the reasoning
// summary text becomes a thinking part. The wire shapes are the ones the
// decoder contract tests pin (streaming reference, O7).
func TestComplete_ToolCallAndReasoning(t *testing.T) {
	var refreshHits int32
	p, _, _ := newBackend(t, noopRefresh(t, &refreshHits), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		mustWriteString(t, w, strings.Join([]string{
			`event: response.created`,
			`data: {"type":"response.created","response":{"id":"resp_tool","model":"gpt-5","status":"in_progress"}}`,
			``,
			`event: response.output_item.added`,
			`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`,
			``,
			`event: response.reasoning_summary_text.delta`,
			`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"delta":"Checking the weather."}`,
			``,
			`event: response.output_item.done`,
			`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`,
			``,
			`event: response.output_item.added`,
			`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"get_weather"}}`,
			``,
			`event: response.function_call_arguments.delta`,
			`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"sequence_number":5,"delta":"{\"location\":"}`,
			``,
			`event: response.function_call_arguments.delta`,
			`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"sequence_number":6,"delta":"\"SF\"}"}`,
			``,
			`event: response.function_call_arguments.done`,
			`data: {"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":1,"arguments":"{\"location\":\"SF\"}"}`,
			``,
			`event: response.output_item.done`,
			`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"location\":\"SF\"}"}}`,
			``,
			`event: response.completed`,
			`data: {"type":"response.completed","response":{"id":"resp_tool","model":"gpt-5","status":"completed","usage":{"input_tokens":21,"output_tokens":8,"total_tokens":29,"output_tokens_details":{"reasoning_tokens":3}}}}`,
			``,
			``,
		}, "\n"))
	})

	resp, err := p.Complete(context.Background(), responsesRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.ID != "resp_tool" || resp.StopReason != ir.StopToolUse {
		t.Errorf("id/stop = %q/%q, want resp_tool/tool_use", resp.ID, resp.StopReason)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("content = %+v, want a thinking part and a tool call", resp.Content)
	}
	if tp, ok := resp.Content[0].(ir.ThinkingPart); !ok || tp.Thinking != "Checking the weather." {
		t.Errorf("content[0] = %+v, want the reasoning summary as a thinking part", resp.Content[0])
	}
	tc, ok := resp.Content[1].(ir.ToolCallPart)
	if !ok {
		t.Fatalf("content[1] = %T, want ir.ToolCallPart", resp.Content[1])
	}
	if tc.ID != "call_1" || tc.Name != "get_weather" || string(tc.Input) != `{"location":"SF"}` {
		t.Errorf("tool call = %+v, want call_1/get_weather with the assembled arguments", tc)
	}
	if resp.Usage.InputTokens != 21 || resp.Usage.OutputTokens != 8 || resp.Usage.ReasoningTokens != 3 {
		t.Errorf("usage = %+v, want 21/8 with 3 reasoning tokens", resp.Usage)
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
		if store, ok := body["store"]; !ok || store != false {
			t.Errorf("request store = %v (present=%v), want false — the backend rejects a body without it", store, ok)
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

// Model discovery (ADR-010): the sync's codex lister refreshes the account's
// catalog from GET {base}/codex/models?client_version=<semver> with the
// codex header set — codex-rs's own refresh path (model-provider/src/
// models_endpoint.rs + protocol/src/openai_models.rs, transcribed into
// testdata/golden/openai-codex/, accessed 2026-10-17). The body is the
// manifest shape {"models":[{"slug",…}]}: reading it as the OpenAI
// {"data":[{"id"}]} list is what left a signed-in account importing nothing.
func TestListModels_CodexCatalog(t *testing.T) {
	body := testutil.LoadGolden(t, "openai-codex/models.json")
	var got struct{ path, version, originator, auth, ua string }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.version = r.URL.Query().Get("client_version")
		got.originator = r.Header.Get("Originator")
		got.auth = r.Header.Get("Authorization")
		got.ua = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		mustWrite(t, w, body)
	}))
	defer srv.Close()

	store := newMemStore()
	seedTokens(t, store, "a1", farFuture())
	auth := codex.NewAuthClient(codex.AuthConfig{Issuer: srv.URL, HTTP: srv.Client(), Store: store, ProviderID: providerID})
	p := codex.New(codex.ProviderConfig{
		ID: providerID, BaseURL: srv.URL,
		Capabilities: provider.Capabilities{Streaming: true, Tools: true},
		Auth:         auth, HTTP: srv.Client(),
	})

	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	var names []string
	for _, m := range models {
		names = append(names, m.Name)
	}
	want := []string{"gpt-5.6-codex", "gpt-5.5", "gpt-5.4-mini"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("ListModels = %v, want the catalog's slugs in order %v", names, want)
	}

	if got.path != "/codex/models" {
		t.Errorf("discovery path = %q, want /codex/models (the codex catalog, not the web app's /models)", got.path)
	}
	if got.version != codex.DefaultClientVersion {
		t.Errorf("client_version = %q, want the pin %q", got.version, codex.DefaultClientVersion)
	}
	if got.originator != "codex_cli_rs" {
		t.Errorf("Originator = %q, want codex_cli_rs", got.originator)
	}
	if got.auth != "Bearer a1" {
		t.Errorf("Authorization = %q, want the account's access token", got.auth)
	}
	if !strings.HasPrefix(got.ua, "codex_cli_rs/proxyllm/") {
		t.Errorf("User-Agent = %q, want the codex_cli_rs/proxyllm prefix", got.ua)
	}
}

// The announced version must stay a plain semver: the backend gates the
// catalog on it, and codex-rs sends MAJOR.MINOR.PATCH (client_version_to_whole).
func TestDefaultClientVersion_IsSemver(t *testing.T) {
	if !semverRe.MatchString(codex.DefaultClientVersion) {
		t.Errorf("DefaultClientVersion = %q, want MAJOR.MINOR.PATCH", codex.DefaultClientVersion)
	}
}

// A deployment pinned to an older client announces its own version.
func TestListModels_ClientVersionOverride(t *testing.T) {
	var version string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		version = r.URL.Query().Get("client_version")
		mustWriteString(t, w, `{"models":[{"slug":"gpt-5.4-mini"}]}`)
	}))
	defer srv.Close()

	store := newMemStore()
	seedTokens(t, store, "a1", farFuture())
	p := codex.New(codex.ProviderConfig{
		ID: providerID, BaseURL: srv.URL, ClientVersion: "0.100.0",
		Auth: codex.NewAuthClient(codex.AuthConfig{Issuer: srv.URL, HTTP: srv.Client(), Store: store, ProviderID: providerID}),
		HTTP: srv.Client(),
	})
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if version != "0.100.0" {
		t.Errorf("client_version = %q, want the configured override 0.100.0", version)
	}
	if len(models) != 1 || models[0].Name != "gpt-5.4-mini" {
		t.Errorf("ListModels = %+v, want the single catalog entry", models)
	}
}

// An EMPTY catalog is a skip, never an empty model list: the backend answers
// a stale client_version with no models, and taking that at face value is
// what would let one sync run delete every synced codex row (the sync deletes
// rows the upstream no longer lists).
func TestListModels_EmptyCatalogIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mustWriteString(t, w, `{"models":[]}`)
	}))
	defer srv.Close()

	store := newMemStore()
	seedTokens(t, store, "a1", farFuture())
	p := codex.New(codex.ProviderConfig{
		ID: providerID, BaseURL: srv.URL,
		Auth: codex.NewAuthClient(codex.AuthConfig{Issuer: srv.URL, HTTP: srv.Client(), Store: store, ProviderID: providerID}),
		HTTP: srv.Client(),
	})
	_, err := p.ListModels(context.Background())
	if err == nil {
		t.Fatal("an empty catalog must surface as an error, not as zero models")
	}
	if !strings.Contains(err.Error(), "client_version") {
		t.Errorf("error %q should name the client_version gate so the operator can act on it", err)
	}
}
