package httpapi

// Wire-name resolution contract tests (2026-09-21 model identity): a
// prefixed routing ID ("myhost/glm-5.3") resolves to the upstream wire name
// ("glm-5.3") for dispatch — through the models-table row when present,
// through the last-slash fallback otherwise. The client-facing response
// echoes the routing ID (OP-2), and the route_trace records the resolution
// (model_resolved event, version 4).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/testutil/mockprovider"
)

// wireEnv assembles one env whose rule routes the prefixed ID via mock-main
// and records the upstream request body for assertion.
func wireEnv(t *testing.T, modelRow *storage.Model) *env {
	t.Helper()
	e := newEnvWithRules(t, routing.Rule{
		MatchModel: "myhost/glm-5.3",
		Chain:      []string{"mock-main"},
		Priority:   100,
		Enabled:    true,
	})
	if modelRow != nil {
		if err := e.store.PutModel(context.Background(), *modelRow); err != nil {
			t.Fatalf("seed model: %v", err)
		}
	}
	return e
}

func TestWireModel_RowNameWins(t *testing.T) {
	e := wireEnv(t, &storage.Model{
		ID: "myhost/glm-5.3", ProviderID: "mock-main", Name: "glm-5.3-wire",
	})
	e.mock.Enqueue(mockprovider.NewScript().JSONResponse(200, `{"id":"x","object":"chat.completion","created":1,"model":"glm-5.3-wire","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil))

	status, body := e.post(t, "/v1/chat/completions",
		`{"model":"myhost/glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	if status != 200 {
		t.Fatalf("status %d body %s", status, body)
	}

	rec := e.mock.Requests()[0]
	var upstream struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(rec.Body), &upstream); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if upstream.Model != "glm-5.3-wire" {
		t.Errorf("upstream model = %q, want the row's wire name glm-5.3-wire", upstream.Model)
	}

	// OP-2: the client sees the routing ID, not the wire name.
	if !strings.Contains(body, `"model":"myhost/glm-5.3"`) {
		t.Errorf("client echo missing routing ID: %s", body)
	}
}

func TestWireModel_LastSlashFallbackWithoutRow(t *testing.T) {
	e := wireEnv(t, nil)
	e.mock.Enqueue(mockprovider.NewScript().JSONResponse(200, `{"id":"x","object":"chat.completion","created":1,"model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil))

	status, _ := e.post(t, "/v1/chat/completions",
		`{"model":"myhost/glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	if status != 200 {
		t.Fatalf("status %d", status)
	}

	rec := e.mock.Requests()[0]
	var upstream struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(rec.Body), &upstream); err != nil {
		t.Fatal(err)
	}
	if upstream.Model != "glm-5.3" {
		t.Errorf("upstream model = %q, want last-slash fallback glm-5.3", upstream.Model)
	}
}

func TestWireModel_LegacyUnprefixedIDUnchanged(t *testing.T) {
	// A legacy unprefixed ID must keep routing exactly as before: the
	// model string reaches the upstream unchanged.
	e := newEnvWithRules(t, routing.Rule{
		MatchModel: "legacy-model",
		Chain:      []string{"mock-main"},
		Priority:   100,
		Enabled:    true,
	})
	e.mock.Enqueue(mockprovider.NewScript().JSONResponse(200, `{"id":"x","object":"chat.completion","created":1,"model":"legacy-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil))

	status, _ := e.post(t, "/v1/chat/completions",
		`{"model":"legacy-model","messages":[{"role":"user","content":"hi"}]}`)
	if status != 200 {
		t.Fatalf("status %d", status)
	}

	rec := e.mock.Requests()[0]
	var upstream struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(rec.Body), &upstream); err != nil {
		t.Fatal(err)
	}
	if upstream.Model != "legacy-model" {
		t.Errorf("upstream model = %q, want unchanged legacy-model", upstream.Model)
	}
}

func TestWireModel_TraceRecordsResolution(t *testing.T) {
	e := wireEnv(t, &storage.Model{
		ID: "myhost/glm-5.3", ProviderID: "mock-main", Name: "glm-5.3-wire",
	})
	e.mock.Enqueue(mockprovider.NewScript().JSONResponse(200, `{"id":"x","object":"chat.completion","created":1,"model":"glm-5.3-wire","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, nil))

	if _, body := e.post(t, "/v1/chat/completions",
		`{"model":"myhost/glm-5.3","messages":[{"role":"user","content":"hi"}]}`); false {
		t.Fatal(body)
	}

	logs, _, err := e.store.ListRequestLogsPage(context.Background(), storage.RequestLogFilter{}, 10, 0)
	if err != nil || len(logs) != 1 {
		t.Fatalf("logs = %d (err %v), want 1", len(logs), err)
	}
	var trace struct {
		Version int `json:"version"`
		Events  []struct {
			Type string `json:"type"`
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(logs[0].RouteTrace), &trace); err != nil {
		t.Fatalf("route_trace: %v", err)
	}
	if trace.Version != 4 {
		t.Errorf("version = %d, want 4", trace.Version)
	}
	seen := false
	for _, ev := range trace.Events {
		if ev.Type == "model_resolved" && ev.From == "myhost/glm-5.3" && ev.To == "glm-5.3-wire" {
			seen = true
		}
	}
	if !seen {
		t.Errorf("model_resolved event missing from trace: %+v", trace.Events)
	}
}
