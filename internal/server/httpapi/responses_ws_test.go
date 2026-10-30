package httpapi

// WebSocket transport tests for GET /v1/responses. The HTTP surface is
// covered by responses_test.go; these tests pin the Codex CLI path: a
// response.create frame streams Responses events as JSON text frames, and a
// second response.create with previous_response_id sees the first turn.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestResponsesWebSocket_TwoTurns(t *testing.T) {
	e := newEnv(t)
	e.mock.Enqueue(mockSSE(t, string(golden(t, "openai/chat-stream.sse"))))
	e.mock.Enqueue(mockSSE(t, string(golden(t, "openai/chat-stream.sse"))))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/v1/responses"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + e.key}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	first := `{"type":"response.create","model":"gpt-5","input":"Hello","stream":true}`
	if err := conn.Write(ctx, websocket.MessageText, []byte(first)); err != nil {
		t.Fatalf("write first: %v", err)
	}
	events, responseID := readWSEvents(t, ctx, conn)
	if responseID == "" || !strings.HasPrefix(responseID, "resp_") {
		t.Fatalf("response.created id = %q, want a resp_ id", responseID)
	}
	if got := joinedTextDeltas(events); !strings.Contains(got, "Hello") || !strings.Contains(got, " world") {
		t.Errorf("relayed text = %q, want the upstream Hello/world deltas", got)
	}
	if completed := lastEvent(events); completed == nil || completed["type"] != "response.completed" {
		t.Fatalf("last event = %v, want response.completed", lastEvent(events))
	}

	second := `{"type":"response.create","model":"gpt-5","input":"Again","previous_response_id":"` + responseID + `","stream":true}`
	if err := conn.Write(ctx, websocket.MessageText, []byte(second)); err != nil {
		t.Fatalf("write second: %v", err)
	}
	_, responseID2 := readWSEvents(t, ctx, conn)
	if responseID2 == "" || responseID2 == responseID {
		t.Errorf("second response id = %q, want a fresh id (first %q)", responseID2, responseID)
	}

	reqs := e.mock.Requests()
	if len(reqs) != 2 {
		t.Fatalf("upstream requests = %d, want 2", len(reqs))
	}
	body := reqs[1].Body
	if !strings.Contains(body, "Hello") || !strings.Contains(body, " world") {
		t.Errorf("second upstream body does not carry the first turn: %s", body)
	}
	if !strings.Contains(body, "Again") {
		t.Errorf("second upstream body does not carry the new input: %s", body)
	}
}

func TestResponsesWebSocket_PlainGetIs405(t *testing.T) {
	e := newEnv(t)
	status, _ := e.get(t, "/v1/responses")
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", status)
	}
}

func TestResponsesWebSocket_RejectsUnknownFrame(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/v1/responses"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + e.key}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"nonsense"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var ev struct {
		Type string `json:"type"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("frame not JSON: %v (%s)", err, data)
	}
	if ev.Type != "error" || ev.Code == "" {
		t.Fatalf("frame = %s, want a typed error event", data)
	}
}

func TestResponsesWebSocket_UnknownPreviousID(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/v1/responses"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + e.key}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	frame := `{"type":"response.create","model":"gpt-5","input":"hi","previous_response_id":"resp_missing"}`
	if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var ev struct {
		Type string `json:"type"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("frame not JSON: %v (%s)", err, data)
	}
	if ev.Type != "error" || ev.Code != "not_found_error" {
		t.Fatalf("frame = %s, want not_found_error", data)
	}
	if got := len(e.mock.Requests()); got != 0 {
		t.Fatalf("upstream requests = %d, want 0 for an unknown previous id", got)
	}
}

// readWSEvents reads text frames until response.completed (or a terminal
// error). It returns the events and the response id from response.created.
func readWSEvents(t *testing.T, ctx context.Context, conn *websocket.Conn) ([]map[string]any, string) {
	t.Helper()
	var events []map[string]any
	var responseID string
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal(data, &ev); err != nil {
			t.Fatalf("frame not JSON: %v (%s)", err, data)
		}
		events = append(events, ev)
		switch ev["type"] {
		case "response.created":
			if resp, ok := ev["response"].(map[string]any); ok {
				responseID, _ = resp["id"].(string)
			}
		case "response.completed":
			return events, responseID
		case "error":
			t.Fatalf("terminal error event: %s", data)
		}
	}
}

func joinedTextDeltas(events []map[string]any) string {
	var b strings.Builder
	for _, ev := range events {
		if ev["type"] != "response.output_text.delta" {
			continue
		}
		if d, ok := ev["delta"].(string); ok {
			b.WriteString(d)
		}
	}
	return b.String()
}

func lastEvent(events []map[string]any) map[string]any {
	if len(events) == 0 {
		return nil
	}
	return events[len(events)-1]
}
