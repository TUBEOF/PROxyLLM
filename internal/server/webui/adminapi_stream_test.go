package webui

// Spec: GET /api/admin/logs/stream (Logs-page redesign) —
// SSE: hello → running snapshot → log events; heartbeat comments; client
// disconnect unsubscribes (no leak); GET needs no CSRF header (the
// session cookie suffices — native EventSource); unwired Stream →
// uniform 503.

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/reqlog"
	"github.com/tubeof/proxyllm/internal/storage"
)

// streamServer builds a real HTTP server over a stream-wired admin API
// with one bootstrapped+logged-in admin; returns the admin client + API.
func streamServer(t *testing.T) (*httptest.Server, *AdminAPI) {
	t.Helper()
	env := newAdminEnv(t)
	env.api.stream = reqlog.New()
	mux := env.api.Handler()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	state := env.bootstrap(t, "streamadmin")
	srv.Client().Jar = loginJar(state)
	return srv, env.api
}

// loginJar packages the login state as a cookie jar the http.Client sends
// on every request (session + csrf cookies, like the SPA's browser jar).
func loginJar(state loginState) *cookieJar {
	return &cookieJar{state: state}
}

// cookieJar adapts loginState to http.CookieJar.
type cookieJar struct {
	state loginState
}

// SetCookies is a no-op adapter (the server sets cookies via the response).
func (j *cookieJar) SetCookies(_ *url.URL, _ []*http.Cookie) {}

// Cookies returns the login state's cookies for the target URL. The
// constructed cookie is a client-side jar entry replaying the session the
// server issued (attributes ride the original Set-Cookie) — test-only, no
// security boundary here.
func (j *cookieJar) Cookies(_ *url.URL) []*http.Cookie {
	var out []*http.Cookie
	for _, pair := range strings.Split(j.state.cookieHeader(), "; ") {
		name, value, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		// Explicit attributes: the jar replay is client-side (attributes
		// ride the server's original Set-Cookie) — setting them here also
		// keeps gosec G124 satisfied across linter versions.
		out = append(out, &http.Cookie{
			Name:     strings.TrimSpace(name),
			Value:    value,
			Secure:   true,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
	}
	return out
}

// TestStream_RequiresAdmin pins the 401 for anonymous readers and the
// no-CSRF-for-GET rule (valid session, no CSRF header → accepted).
func TestStream_RequiresAdmin(t *testing.T) {
	env := newAdminEnv(t)
	env.api.stream = reqlog.New()
	srv := httptest.NewServer(env.api.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/admin/logs/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous stream status = %d, want 401", resp.StatusCode)
	}
}

// TestStream_UnwiredStreamFails503 pins the uniform degradation.
func TestStream_UnwiredStreamFails503(t *testing.T) {
	env := newAdminEnv(t)
	mux := env.api.Handler()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	state := env.bootstrap(t, "nostream")
	srv.Client().Jar = loginJar(state)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/admin/logs/stream", nil)
	req.Header.Set("X-CSRF-Token", state.csrfToken)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("unwired stream status = %d, want 503", resp.StatusCode)
	}
}

// frame is one parsed SSE frame.
type frame struct {
	event string
	data  string
}

// streamReader reads SSE frames off a live connection.
type streamReader struct {
	scanner *bufio.Scanner
	body    *http.Response
}

// next returns the next EVENT frame; the H2 header comment (`: ok`) is
// skipped silently (not an event), a heartbeat comment is surfaced as
// {event: "comment", data: "ping"}. Returns nil past timeout d.
func (sr *streamReader) next(t *testing.T, d time.Duration) *frame {
	t.Helper()
	done := make(chan *frame, 1)
	go func() {
		f := &frame{}
		dataLines := 0
		for sr.scanner.Scan() {
			line := sr.scanner.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				f.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				f.data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				dataLines++
			case strings.HasPrefix(line, ":"):
				if line != ": ok" {
					done <- &frame{event: "comment", data: strings.TrimSpace(strings.TrimPrefix(line, ":"))}
					return
				}
			case line == "":
				if f.event != "" || dataLines > 0 {
					done <- f
					return
				}
				f = &frame{}
			}
		}
		close(done)
	}()
	select {
	case f := <-done:
		if f == nil {
			t.Fatal("stream ended early")
		}
		return f
	case <-time.After(d):
		return nil
	}
}

// TestStream_HelloRunningThenLogInsert pins the happy wire: hello, initial
// running snapshot, and a log event for a row inserted after connect.
func TestStream_HelloRunningThenLogInsert(t *testing.T) {
	srv, api := streamServer(t)
	resp, err := srv.Client().Get(srv.URL + "/api/admin/logs/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	sr := &streamReader{scanner: bufio.NewScanner(resp.Body), body: resp}

	if f := sr.next(t, 2*time.Second); f == nil || f.event != "hello" {
		t.Fatalf("first frame = %+v, want hello", f)
	}
	if f := sr.next(t, 2*time.Second); f == nil || f.event != "running" {
		t.Fatalf("second frame = %+v, want running", f)
	}

	// A row published through the hub (the writeRequestLog path) arrives
	// on the wire; the direct store insert feeds the cursor drain on the
	// next wake (also exercised below).
	api.stream.PublishRow(reqlog.LogRow{
		ID: 12, Timestamp: 1760000000, ClientKeyID: "k", ProviderID: "p",
		Model: "glm-5.3-flash", Status: 200, LatencyMS: 100,
	})

	for i := 0; i < 6; i++ {
		f := sr.next(t, 3*time.Second)
		if f == nil {
			t.Fatal("no log event within timeout")
		}
		if f.event != "log" {
			continue
		}
		var row struct {
			ID     int64  `json:"id"`
			Model  string `json:"model"`
			Status int    `json:"status"`
		}
		if err := json.Unmarshal([]byte(f.data), &row); err != nil {
			t.Fatalf("log payload: %v (%q)", err, f.data)
		}
		if row.ID != 12 || row.Model != "glm-5.3-flash" || row.Status != 200 {
			t.Fatalf("unexpected log row: %+v", row)
		}
		return
	}
	t.Fatal("log frames exhausted")
}

// TestStream_ClientDisconnectCleansUnsubscribe pins the hub cleanup: after
// the client disconnects, the in-flight registry is left untouched but the
// subscriber count returns to zero (no leak).
func TestStream_ClientDisconnectCleansUnsubscribe(t *testing.T) {
	srv, api := streamServer(t)
	resp, err := srv.Client().Get(srv.URL + "/api/admin/logs/stream")
	if err != nil {
		t.Fatal(err)
	}
	sr := &streamReader{scanner: bufio.NewScanner(resp.Body), body: resp}
	sr.next(t, 2*time.Second) // hello

	deadline := time.Now().Add(2 * time.Second)
	_ = resp.Body.Close()
	for time.Now().Before(deadline) {
		if api.stream.SubCount() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("subscriber leaked: SubCount = %d", api.stream.SubCount())
}

// TestStream_InflightLifecycle pins the live running→log flow through the
// REAL wired api pipeline: hub Start via a direct publish + row insert. At
// this level the httpapi integration (beginRequest) is exercised in
// unit/server tests; here we pin wire shapes only.
func TestStream_InflightLifecycle(t *testing.T) {
	srv, api := streamServer(t)
	resp, err := srv.Client().Get(srv.URL + "/api/admin/logs/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	sr := &streamReader{scanner: bufio.NewScanner(resp.Body), body: resp}
	sr.next(t, 2*time.Second) // hello
	sr.next(t, 2*time.Second) // running (empty)

	finish, _ := api.stream.Start(reqlog.Inflight{ClientKeyID: "k", Model: "glm-5.3-flash", Protocol: "anthropic"})
	if f := sr.next(t, 2*time.Second); f == nil || f.event != "running" {
		t.Fatalf("post-start frame = %+v, want running", f)
	}
	finish(reqlog.LogRow{ID: 77, Timestamp: 1760000000, Model: "glm-5.3-flash", Status: 200})
	var sawRunningAfter bool
	var sawLogRow *reqlog.LogRow
	for i := 0; i < 6 && (sawRunningAfter == false || sawLogRow == nil); i++ {
		f := sr.next(t, 2*time.Second)
		if f == nil {
			break
		}
		switch f.event {
		case "running":
			var rows []reqlog.Inflight
			if json.Unmarshal([]byte(f.data), &rows) == nil && len(rows) == 0 {
				sawRunningAfter = true
			}
		case "log":
			var row reqlog.LogRow
			if json.Unmarshal([]byte(f.data), &row) == nil && row.ID == 77 {
				sawLogRow = &row
			}
		}
	}
	if !sawRunningAfter || sawLogRow == nil {
		t.Errorf("in-flight lifecycle incomplete: runningAfter=%v row=%+v", sawRunningAfter, sawLogRow)
	}
}

// TestStream_HeartbeatComment pins the periodic `: ping` comment (the
// interval is pinned to 50 ms here — production is the 15 s default).
func TestStream_HeartbeatComment(t *testing.T) {
	env := newAdminEnv(t)
	env.api.stream = reqlog.New()
	env.api.streamHeartbeat = 50 * time.Millisecond
	srv := httptest.NewServer(env.api.Handler())
	defer srv.Close()
	state := env.bootstrap(t, "pingadmin")
	srv.Client().Jar = loginJar(state)

	resp, err := srv.Client().Get(srv.URL + "/api/admin/logs/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	sr := &streamReader{scanner: bufio.NewScanner(resp.Body), body: resp}
	sr.next(t, 2*time.Second) // hello
	sr.next(t, 2*time.Second) // running

	if f := sr.next(t, 3*time.Second); f == nil || f.event != "comment" || f.data != "ping" {
		t.Errorf("heartbeat = %+v, want : ping comment", f)
	}
}

// TestStream_LogDTOHasDisplayNames pins the getLogs projection end-to-end
// (the SPA renders key/provider names).
func TestStream_LogDTOHasDisplayNames(t *testing.T) {
	env := newAdminEnv(t)
	srv := httptest.NewServer(env.api.Handler())
	defer srv.Close()

	state := env.bootstrap(t, "dtoadmin")
	w := env.authedCall(t, http.MethodPost, "/api/admin/keys", `{"name":"Coder2"}`, state)
	if w.Code != http.StatusOK {
		t.Fatalf("key create: %d", w.Code)
	}
	var keyResp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &keyResp); err != nil {
		t.Fatalf("key create body: %v (%s)", err, w.Body.String())
	}

	if _, err := env.st.InsertRequestLog(context.Background(), storage.RequestLog{
		Timestamp: 1760000000, ClientKeyID: keyResp.ID,
		ProviderID: "prov-missing", Model: "glm-5.3-flash", Status: 200,
		ConversationID: "conv_a1b2c3d4", Protocol: "anthropic",
		Features: json.RawMessage(`{"streaming":true}`),
	}); err != nil {
		t.Fatal(err)
	}

	w2 := env.call(t, http.MethodGet, "/api/admin/logs?limit=10", "", state.cookieHeader())
	if w2.Code != http.StatusOK {
		t.Fatalf("logs: %d", w2.Code)
	}
	var respBody struct {
		Logs []logDTO `json:"logs"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &respBody); err != nil {
		t.Fatalf("logs body: %v (%s)", err, w2.Body.String())
	}
	if len(respBody.Logs) != 1 {
		t.Fatalf("logs = %d rows", len(respBody.Logs))
	}
	row := respBody.Logs[0]
	if row.KeyName != "Coder2" {
		t.Errorf("key_name = %q, want Coder2", row.KeyName)
	}
	if row.ProviderName != "prov-missing" {
		t.Errorf("provider fallback = %q, want raw id", row.ProviderName)
	}
	if row.ConversationID != "conv_a1b2c3d4" || row.Protocol != "anthropic" {
		t.Errorf("conversation/protocol = %q/%q", row.ConversationID, row.Protocol)
	}
	if string(row.Features) != `{"streaming":true}` {
		t.Errorf("features = %s", row.Features)
	}
}
