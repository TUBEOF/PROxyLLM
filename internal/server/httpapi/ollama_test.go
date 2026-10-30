package httpapi

// Native Ollama surface tests: POST /api/chat (JSON and NDJSON) and
// GET /api/tags through the real proxy stack.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestOllamaChat_NonStreaming(t *testing.T) {
	e := newEnv(t)
	e.mock.Enqueue(mockJSON(t, http.StatusOK, string(golden(t, "openai/chat-response.json"))))

	status, body := e.post(t, "/api/chat",
		`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}
	var resp struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		Done bool `json:"done"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("response not JSON: %v (%s)", err, body)
	}
	if !resp.Done || resp.Message.Content == "" {
		t.Fatalf("response = %+v, want done with content", resp)
	}
	upstream := e.mock.Requests()
	if len(upstream) != 1 {
		t.Fatalf("upstream requests = %d", len(upstream))
	}
	if !strings.Contains(upstream[0].Body, `"model":"gpt-5"`) {
		t.Errorf("upstream body = %s, want the chat wire model", upstream[0].Body)
	}
}

func TestOllamaChat_StreamingNDJSON(t *testing.T) {
	e := newEnv(t)
	e.mock.Enqueue(mockSSE(t, string(golden(t, "openai/chat-stream.sse"))))

	status, body := e.postStream(t, "/api/chat",
		`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}
	var sawContent, sawDone bool
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if line == "" {
			continue
		}
		var ev struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Done bool `json:"done"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("NDJSON line not JSON: %v (%s)", err, line)
		}
		if ev.Message.Content != "" {
			sawContent = true
		}
		if ev.Done {
			sawDone = true
		}
	}
	if !sawContent || !sawDone {
		t.Fatalf("NDJSON stream: content=%v done=%v", sawContent, sawDone)
	}
}

func TestOllamaTags_ListsModels(t *testing.T) {
	e := newEnv(t)
	e.seedModel(t, "gpt-5", nil)
	status, body := e.get(t, "/api/tags")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}
	var resp struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("response not JSON: %v (%s)", err, body)
	}
	for _, m := range resp.Models {
		if m.Name == "gpt-5" {
			return
		}
	}
	t.Fatalf("models = %+v, want gpt-5", resp.Models)
}

func TestOllamaChat_UnauthorizedShape(t *testing.T) {
	e := newEnv(t)
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/api/chat",
		strings.NewReader(`{"model":"gpt-5","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	status, body := e.do(t, req)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if !strings.HasPrefix(strings.TrimSpace(body), `{"error":`) {
		t.Fatalf("body = %s, want Ollama error shape", body)
	}
}
