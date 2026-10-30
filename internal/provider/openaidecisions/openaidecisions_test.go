package openaidecisions

// Provider-wire tests: the Decisions provider must POST /v1/decisions with
// Bearer auth, decode the golden decision response, and refuse chat traffic.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/openai"
)

func TestDecide_SendsDecisionsWire(t *testing.T) {
	reqRaw, err := os.ReadFile("../../../testdata/golden/openai/decisions-request.json")
	if err != nil {
		t.Fatalf("read request golden: %v", err)
	}
	req, err := openai.DecisionsCodec{}.DecodeRequest(reqRaw)
	if err != nil {
		t.Fatalf("decode request golden: %v", err)
	}
	respRaw, err := os.ReadFile("../../../testdata/golden/openai/decisions-response.json")
	if err != nil {
		t.Fatalf("read response golden: %v", err)
	}
	var gotAuth, gotPath string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(respRaw)
	}))
	t.Cleanup(srv.Close)

	p := New("acc-decisions", srv.URL, "test-key", provider.Capabilities{Decision: true})
	resp, err := p.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotPath != "/v1/decisions" {
		t.Errorf("path = %q, want /v1/decisions", gotPath)
	}
	if body["model"] == nil {
		t.Errorf("upstream body has no model: %v", body)
	}
	if len(resp.Answers) == 0 {
		t.Errorf("decoded decision has no answers: %+v", resp)
	}
}

func TestComplete_RefusesChat(t *testing.T) {
	p := New("acc-decisions", "https://api.example.com", "test-key", provider.Capabilities{Decision: true})
	if _, err := p.Complete(context.Background(), ir.Request{}); !errors.Is(err, ErrNotChat) {
		t.Fatalf("Complete error = %v, want ErrNotChat", err)
	}
	if _, _, err := p.Stream(context.Background(), ir.Request{}); !errors.Is(err, ErrNotChat) {
		t.Fatalf("Stream error = %v, want ErrNotChat", err)
	}
}
