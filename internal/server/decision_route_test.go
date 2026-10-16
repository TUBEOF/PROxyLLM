package server

// Spec-derived decision-surface tests (decision models): POST /v1/systemone
// and POST /v1/decisions route by model with the `decision` capability
// required, dispatch through provider.Decider, and answer in the surface's
// own shape — either surface reaches the same upstream through the IR, so
// the two fixtures (systemone and the OpenAI Decisions API shapes) must
// produce the same routed request. The fixtures in testdata/golden/ are the
// specification (provenance: docs.ollama.com/api/systemone and
// developers.openai.com/api/docs/guides/decisions, accessed 2026-10-16).

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/openai"
	"github.com/tubeof/proxyllm/internal/translate/systemone"
	"github.com/tubeof/proxyllm/internal/translate/testutil"
)

// decisionStub is a decision-capable account: a provider.Provider that
// records every IR request it receives and answers with one canned IR
// response. The chat surface is deliberately dead — a decision route must
// never touch it.
type decisionStub struct {
	id   string
	caps provider.Capabilities

	mu       sync.Mutex
	requests []ir.DecisionRequest
	resp     ir.DecisionResponse
}

func (s *decisionStub) ID() string                               { return s.id }
func (s *decisionStub) Capabilities() provider.Capabilities      { return s.caps }
func (s *decisionStub) RefreshCredentials(context.Context) error { return nil }
func (s *decisionStub) Complete(context.Context, ir.Request) (ir.Response, error) {
	return ir.Response{}, errors.New("decisionStub: no chat surface")
}
func (s *decisionStub) Stream(context.Context, ir.Request) (<-chan ir.StreamEvent, func(), error) {
	return nil, func() {}, errors.New("decisionStub: no chat surface")
}

func (s *decisionStub) Decide(_ context.Context, req ir.DecisionRequest) (ir.DecisionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	return s.resp, nil
}

func (s *decisionStub) captured() []ir.DecisionRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ir.DecisionRequest(nil), s.requests...)
}

func TestServer_DecisionSurfacesTranslateBothWays(t *testing.T) {
	ctx := context.Background()
	cfg := newConfig(t)

	st, err := storage.Open(cfg.Storage)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	if err := st.EnsureProvider(ctx, "mock-dec", "custom-openai", "Decision Mock"); err != nil {
		t.Fatalf("ensure provider: %v", err)
	}
	// The model row carries the wire name: the routing identity "clef"
	// resolves to the upstream's "clef:cloud" before dispatch.
	if err := st.PutModel(ctx, storage.Model{ID: "clef", ProviderID: "mock-dec", Name: "clef:cloud"}); err != nil {
		t.Fatalf("put model: %v", err)
	}
	if err := st.PutRoutingRule(ctx, storage.RoutingRule{
		ID: "r-dec", MatchModel: "clef", Chain: []string{"mock-dec"}, Priority: 100, Enabled: true,
	}); err != nil {
		t.Fatalf("put rule: %v", err)
	}

	// The canned answer is the documented systemone response fixture,
	// decoded through the codec — the upstream is spec-shaped, not an
	// invention.
	canned, err := systemone.Codec{}.DecodeResponse(testutil.LoadGolden(t, "systemone/response.json"))
	if err != nil {
		t.Fatalf("decode canned response: %v", err)
	}
	stub := &decisionStub{id: "mock-dec", caps: provider.Capabilities{Decision: true}, resp: canned}

	srv, err := New(&cfg, st, nil, WithProvider(stub), WithMasterKey(wiringMasterKey(t)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	_, key, err := authCreateKey(ctx, st)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	post := func(path string, body []byte) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, ensureBodyClosed(t, resp)
	}

	// The wire bodies keep the fixtures' documented shape; only the model
	// field becomes the proxy-side routing identity ("clef") — the row
	// above resolves it back to the upstream's wire name.
	systemOneBody := func() []byte {
		t.Helper()
		req, err := systemone.Codec{}.DecodeRequest(testutil.LoadGolden(t, "systemone/request.json"))
		if err != nil {
			t.Fatalf("decode fixture: %v", err)
		}
		req.Model = "clef"
		out, err := systemone.Codec{}.EncodeRequest(req)
		if err != nil {
			t.Fatalf("encode fixture: %v", err)
		}
		return out
	}
	decisionsBody := func() []byte {
		t.Helper()
		req, err := openai.DecisionsCodec{}.DecodeRequest(testutil.LoadGolden(t, "openai/decisions-request.json"))
		if err != nil {
			t.Fatalf("decode fixture: %v", err)
		}
		req.Model = "clef"
		out, err := openai.DecisionsCodec{}.EncodeRequest(req)
		if err != nil {
			t.Fatalf("encode fixture: %v", err)
		}
		return out
	}

	// Surface 1: the systemone shape passes through to the systemone
	// upstream; the answer comes back in the same shape.
	status, body := post("/v1/systemone", systemOneBody())
	if status != http.StatusOK {
		t.Fatalf("systemone: status %d, body %s", status, body)
	}
	for _, want := range []string{`"choice":"bug"`, `"noul":0.92`, `"input_tokens":174`, `"model":"clef"`} {
		if !strings.Contains(body, want) {
			t.Errorf("systemone answer missing %s: %s", want, body)
		}
	}

	// Surface 2: the OpenAI Decisions shape reaches the SAME upstream —
	// translated through the IR — and the answer comes back in the
	// decisions shape (an answers array, numeric score indices).
	status, body = post("/v1/decisions", decisionsBody())
	if status != http.StatusOK {
		t.Fatalf("decisions: status %d, body %s", status, body)
	}
	for _, want := range []string{`"answers"`, `"type":"choice"`, `"value":"bug"`, `"type":"score"`} {
		if !strings.Contains(body, want) {
			t.Errorf("decisions answer missing %s: %s", want, body)
		}
	}

	// Both surfaces produced the same routed request: the wire-name
	// resolution ran, the state survived, and the questions arrived typed.
	got := stub.captured()
	if len(got) != 2 {
		t.Fatalf("upstream saw %d decision requests, want 2", len(got))
	}
	for i, req := range got {
		if req.Model != "clef:cloud" {
			t.Errorf("request %d: upstream model = %q, want the row's wire name clef:cloud", i, req.Model)
		}
		if req.State != "Our checkout has returned 500 errors since 9am." {
			t.Errorf("request %d: state = %q, want the fixture's evidence text", i, req.State)
		}
		if len(req.Questions) != 3 {
			t.Errorf("request %d: %d questions, want 3 — both shapes carry all three", i, len(req.Questions))
		}
	}

	// The log rows are observable like every request: the decision
	// surface's protocol value and the `decision` feature.
	logs, err := st.ListRequestLogs(ctx, 10)
	if err != nil || len(logs) < 2 {
		t.Fatalf("request logs unreadable (%v) or fewer than 2 rows", err)
	}
	if logs[1].Protocol != "systemone" {
		t.Errorf("protocol = %q, want systemone", logs[1].Protocol)
	}
	if !strings.Contains(string(logs[1].Features), `"decision":true`) {
		t.Errorf("features = %s, want decision:true", logs[1].Features)
	}
	if logs[0].Protocol != "decisions" {
		t.Errorf("protocol = %q, want decisions", logs[0].Protocol)
	}
}

// A model whose route cannot serve decisions answers the no-route 404 with
// the model named — the reference's spelling, on both decision surfaces.
func TestServer_DecisionRouteMissingAnswers404(t *testing.T) {
	ctx := context.Background()
	cfg := newConfig(t)
	st, err := storage.Open(cfg.Storage)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	// The account exists but declares no decision capability: the chain
	// filters to empty before dispatch.
	if err := st.EnsureProvider(ctx, "mock-plain", "custom-openai", "Plain Mock"); err != nil {
		t.Fatalf("ensure provider: %v", err)
	}
	if err := st.PutRoutingRule(ctx, storage.RoutingRule{
		ID: "r-plain", MatchModel: "chat-model", Chain: []string{"mock-plain"}, Priority: 100, Enabled: true,
	}); err != nil {
		t.Fatalf("put rule: %v", err)
	}
	stub := &decisionStub{id: "mock-plain", caps: provider.Capabilities{Streaming: true}}

	srv, err := New(&cfg, st, nil, WithProvider(stub), WithMasterKey(wiringMasterKey(t)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	_, key, err := authCreateKey(ctx, st)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	body := `{"model":"chat-model","state":"x","questions":{"q":{"type":"noul","instructions":"i"}}}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/systemone", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	answer := ensureBodyClosed(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404 (no decision-capable route); body %s", resp.StatusCode, answer)
	}
	if !strings.Contains(answer, "model: chat-model") {
		t.Errorf("body = %s, want the model named as \"model: <id>\"", answer)
	}
	if got := stub.captured(); len(got) != 0 {
		t.Errorf("the upstream must not be called for a filtered chain, saw %d requests", len(got))
	}
}
