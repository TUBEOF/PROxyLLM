// Package mockserver drives an established MockServer instance
// (https://www.mock-server.com) as the wire-accurate upstream for the
// conformance matrix. MockServer encodes the provider's own wire format —
// OpenAI Chat and Responses, Anthropic Messages, Gemini, Ollama NDJSON — with
// streaming framing, tool calls, multi-turn scenarios and fault injection, so
// the matrix tests the production transport and codecs without a provider key
// and without a hand-written fake.
//
// The suite never starts a container itself on a developer machine: it uses
// MOCKSERVER_URL when set, otherwise starts MOCKSERVER_JAR with the local JRE
// when that is set, and skips with a clear reason when neither is available.
// CI runs a MockServer service container and sets MOCKSERVER_URL.
package mockserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Server is one MockServer endpoint.
type Server struct {
	URL string
}

// New returns the shared MockServer for this test run. It prefers
// MOCKSERVER_URL, then a JAR started from MOCKSERVER_JAR. Without either it
// skips: the matrix job is the place that owns the dependency.
func New(t *testing.T) *Server {
	t.Helper()
	if url := os.Getenv("MOCKSERVER_URL"); url != "" {
		s := &Server{URL: url}
		s.waitReady(t)
		return s
	}
	if jar := os.Getenv("MOCKSERVER_JAR"); jar != "" {
		return startJar(t, jar)
	}
	t.Skip("MockServer not configured: set MOCKSERVER_URL (CI service container) or MOCKSERVER_JAR (local)")
	return nil
}

// startJar runs the standalone MockServer on a free port.
func startJar(t *testing.T, jar string) *Server {
	t.Helper()
	port := freePort(t)
	// The JAR path comes from the test configuration (MOCKSERVER_JAR), never
	// from request data.
	//nolint:gosec // G702: test-config path, never request data
	cmd := exec.Command("java", "-jar", jar, "-serverPort", fmt.Sprint(port))
	logFile, err := os.CreateTemp(t.TempDir(), "mockserver-*.log")
	if err != nil {
		t.Fatalf("mockserver log: %v", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mockserver: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = logFile.Close()
	})
	s := &Server{URL: fmt.Sprintf("http://127.0.0.1:%d", port)}
	s.waitReady(t)
	return s
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// waitReady polls until the control plane answers.
func (s *Server) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodPut, s.URL+"/mockserver/retrieve?type=REQUESTS&format=JSON", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("mockserver at %s did not become ready", s.URL)
}

// Reset clears expectations and the recorded request log.
func (s *Server) Reset(t *testing.T) {
	t.Helper()
	s.put(t, "/mockserver/reset", nil, http.StatusOK)
}

// Upsert installs one expectation (a JSON object) and fails the test when
// MockServer rejects it.
func (s *Server) Upsert(t *testing.T, expectation any) {
	t.Helper()
	raw, err := json.Marshal(expectation)
	if err != nil {
		t.Fatalf("marshal expectation: %v", err)
	}
	s.put(t, "/mockserver/expectation", raw, http.StatusCreated)
}

// Recorded is one request MockServer received.
type Recorded struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   struct {
		Type string          `json:"type"`
		JSON json.RawMessage `json:"json"`
	} `json:"body"`
}

// Requests returns every request the server received since the last Reset.
func (s *Server) Requests(t *testing.T) []Recorded {
	t.Helper()
	raw := s.put(t, "/mockserver/retrieve?type=REQUESTS&format=JSON", []byte(`{}`), http.StatusOK)
	var out []Recorded
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode recorded requests: %v (%s)", err, raw)
	}
	return out
}

// Verify asserts a request matcher against the recorded log; MockServer
// answers 202 on success and 406 on mismatch.
func (s *Server) Verify(t *testing.T, matcher any) {
	t.Helper()
	raw, err := json.Marshal(matcher)
	if err != nil {
		t.Fatalf("marshal matcher: %v", err)
	}
	body := s.do(t, http.MethodPut, "/mockserver/verify", raw)
	if body.status != http.StatusAccepted {
		t.Fatalf("verify %s: status %d (%s)", raw, body.status, body.payload)
	}
}

type response struct {
	status  int
	payload []byte
}

func (s *Server) put(t *testing.T, path string, body []byte, want int) []byte {
	t.Helper()
	resp := s.do(t, http.MethodPut, path, body)
	if resp.status != want {
		t.Fatalf("%s: status %d, want %d (%s)", path, resp.status, want, resp.payload)
	}
	return resp.payload
}

func (s *Server) do(t *testing.T, method, path string, body []byte) response {
	t.Helper()
	if body == nil {
		body = []byte(`{}`)
	}
	req, err := http.NewRequest(method, s.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return response{status: resp.StatusCode, payload: raw}
}
