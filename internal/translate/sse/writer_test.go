package sse_test

import (
	"bytes"
	"errors"
	"net/http"
	"testing"

	"github.com/tubeof/proxyllm/internal/translate/sse"
)

// SSE writer contract tests, RED-GREEN per Analysis F. The writer must emit
// the WHATWG wire form (facts SSE-1..SSE-7, phase-1 research §1:
// html.spec.whatwg.org/multipage/server-sent-events.html, accessed
// 2026-09-10) and the HTTP/2 comment trick (facts H2-2/H2-3, phase-1
// research §2): a `: ok` comment frame immediately after the header, one
// flush per event. The reader in this package is fixture-verified against
// the same spec, so a round trip through it proves wire conformance.

// recorder is an http.ResponseWriter capturing written bytes and flushes.
type recorder struct {
	buf    bytes.Buffer
	flush  int
	status int
}

func (r *recorder) Header() http.Header         { return http.Header{} }
func (r *recorder) Write(p []byte) (int, error) { return r.buf.Write(p) }
func (r *recorder) WriteHeader(status int)      { r.status = status }
func (r *recorder) Flush()                      { r.flush++ }

// The header comment (H2-2/H2-3): WriteHeader must be followed by the
// spec-ignored `: ok` comment frame and one flush — releasing the HTTP/2
// HEADERS frame behind proxies without disturbing any pending event.
func TestSSEWriter_HeaderComment(t *testing.T) {
	rec := &recorder{}
	w := sse.NewWriter(rec)
	w.WriteHeader(http.StatusOK)

	if rec.status != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.status)
	}
	if got := rec.buf.String(); got != ": ok\n\n" {
		t.Errorf("header frame = %q, want %q (H2-2)", got, ": ok\n\n")
	}
	if rec.flush != 1 {
		t.Errorf("flushes after header = %d, want 1", rec.flush)
	}
}

// Events must emit as `event:` + `data:` lines terminated by the dispatching
// blank line (SSE-5: dispatch on blank line), one flush per event (C2: relay
// as-it-arrives).
func TestSSEWriter_EventFrame(t *testing.T) {
	rec := &recorder{}
	w := sse.NewWriter(rec)
	w.WriteHeader(http.StatusOK)
	rec.buf.Reset()
	rec.flush = 0

	if err := w.Event("message_start", `{"type":"message_start"}`); err != nil {
		t.Fatalf("Event: %v", err)
	}
	if got := rec.buf.String(); got != "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" {
		t.Errorf("event frame = %q", got)
	}
	if rec.flush != 1 {
		t.Errorf("flushes per event = %d, want 1", rec.flush)
	}
}

// Unnamed events (data-only SSE, the OpenAI chat surface) must emit without
// an `event:` line — the dispatch blank line terminates the frame (SSE-5).
func TestSSEWriter_DataOnlyFrame(t *testing.T) {
	rec := &recorder{}
	w := sse.NewWriter(rec)
	w.WriteHeader(http.StatusOK)
	rec.buf.Reset()

	if err := w.Data(`{"delta":"Hi"}`); err != nil {
		t.Fatalf("Data: %v", err)
	}
	if got := rec.buf.String(); got != "data: {\"delta\":\"Hi\"}\n\n" {
		t.Errorf("data frame = %q", got)
	}
}

// The raw sentinel (the OpenAI `data: [DONE]` terminator) must be writable
// verbatim, with the same per-frame blank line and flush.
func TestSSEWriter_RawFrame(t *testing.T) {
	rec := &recorder{}
	w := sse.NewWriter(rec)
	w.WriteHeader(http.StatusOK)
	rec.buf.Reset()
	rec.flush = 0

	if err := w.Raw("data: [DONE]\n\n"); err != nil {
		t.Fatalf("Raw: %v", err)
	}
	if got := rec.buf.String(); got != "data: [DONE]\n\n" {
		t.Errorf("raw frame = %q", got)
	}
	if rec.flush != 1 {
		t.Errorf("flushes = %d, want 1", rec.flush)
	}
}

// Comments (upstream keepalives relayed downstream) must emit as `:` comment
// lines (SSE-3) — spec-ignored by clients but producing a DATA frame that
// resets the client's 300 s silence watchdog (C2: relayed bytes count).
func TestSSEWriter_Comment(t *testing.T) {
	rec := &recorder{}
	w := sse.NewWriter(rec)
	w.WriteHeader(http.StatusOK)
	rec.buf.Reset()
	rec.flush = 0

	if err := w.Comment("keepalive"); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if got := rec.buf.String(); got != ": keepalive\n\n" {
		t.Errorf("comment frame = %q", got)
	}
	if rec.flush != 1 {
		t.Errorf("flushes = %d, want 1", rec.flush)
	}

	// A bare comment (empty text) emits the minimal `:` line.
	rec.buf.Reset()
	if err := w.Comment(""); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if got := rec.buf.String(); got != ":\n\n" {
		t.Errorf("bare comment frame = %q", got)
	}
}

// Multi-line data: the data buffer must never contain a raw newline that
// could split into a forged extra field line — newlines in the payload split
// into repeated `data:` lines per WHATWG SSE-5 (one LF per field on the
// reader side reassembles them).
func TestSSEWriter_MultiLineData(t *testing.T) {
	rec := &recorder{}
	w := sse.NewWriter(rec)
	w.WriteHeader(http.StatusOK)
	rec.buf.Reset()

	if err := w.Event("m", "line1\nline2"); err != nil {
		t.Fatalf("Event: %v", err)
	}
	if got := rec.buf.String(); got != "event: m\ndata: line1\ndata: line2\n\n" {
		t.Errorf("multi-line frame = %q", got)
	}
}

// Round trip through this package's spec-verified Reader proves wire
// conformance end to end (SSE-1..SSE-7).
func TestSSEWriter_RoundTrip(t *testing.T) {
	rec := &recorder{}
	w := sse.NewWriter(rec)
	w.WriteHeader(http.StatusOK)

	frames := []struct {
		name string
		data string
	}{
		{"message_start", `{"a":1}`},
		{"content_block_delta", `{"b":"\n"}`},
		{"message_stop", `{"c":3}`},
	}
	for _, f := range frames {
		if err := w.Event(f.name, f.data); err != nil {
			t.Fatalf("Event(%s): %v", f.name, err)
		}
	}
	if err := w.Comment("ka"); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if err := w.Data(`{"d":4}`); err != nil {
		t.Fatalf("Data: %v", err)
	}

	got := []sse.Event{}
	r := sse.NewReader(bytes.NewReader(rec.buf.Bytes()))
	for {
		ev, err := r.Next()
		if err != nil {
			break
		}
		got = append(got, ev)
	}
	if len(got) != 6 {
		t.Fatalf("events = %+v, want 6 (header comment + 3 named + 1 comment + 1 unnamed)", got)
	}
	// want pairs carry the payload in Comment for comment events (the
	// reader's Event puts comment text in Comment, not Data).
	want := []struct {
		name    string
		data    string
		comment bool
	}{
		// The H2-2 header comment is part of the wire stream (SSE-3). The
		// reader surfaces comment text raw after ":" (SSE-4's one-space strip
		// applies to field values), so payloads keep the separator space.
		{"", " ok", true},
		{"message_start", `{"a":1}`, false},
		{"content_block_delta", `{"b":"\n"}`, false},
		{"message_stop", `{"c":3}`, false},
		{"", " ka", true},
		{"", `{"d":4}`, false},
	}
	for i, wn := range want {
		if got[i].IsComment != wn.comment || got[i].Name != wn.name ||
			(wn.comment && got[i].Comment != wn.data) ||
			(!wn.comment && got[i].Data != wn.data) {
			t.Errorf("event[%d] = %+v, want name=%q payload=%q comment=%v", i, got[i], wn.name, wn.data, wn.comment)
		}
	}
}

// Sink errors must propagate from every write path — the relay layer maps
// them to the client disconnect, cancelling the upstream (C2).
func TestSSEWriter_WriteErrors(t *testing.T) {
	sink := &failWriter{}
	w := sse.NewWriter(sink)

	for name, fn := range map[string]func() error{
		"event":   func() error { return w.Event("m", "{}") },
		"data":    func() error { return w.Data("{}") },
		"comment": func() error { return w.Comment("ka") },
		"raw":     func() error { return w.Raw("data: [DONE]\n\n") },
	} {
		if err := fn(); err == nil {
			t.Errorf("%s: write error must propagate", name)
		}
	}
}

// failWriter fails every write.
type failWriter struct{}

func (f *failWriter) Write(p []byte) (int, error) { return 0, errors.New("boom") }
func (f *failWriter) WriteHeader(int)             {}
func (f *failWriter) Flush()                      {}

// Writing to a bare io.Writer (no http.Flusher) must succeed without
// flushing — the flush step is conditional (C2 applies to HTTP sinks).
func TestSSEWriter_BareWriter(t *testing.T) {
	var buf bytes.Buffer
	w := sse.NewWriter(&buf)
	w.WriteHeader(http.StatusOK) // no-op without http.ResponseWriter
	if err := w.Data("{}"); err != nil {
		t.Fatalf("Data: %v", err)
	}
	if got := buf.String(); got != ": ok\n\ndata: {}\n\n" {
		t.Errorf("wire = %q", got)
	}
}
