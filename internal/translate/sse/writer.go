// The SSE writer side of this package: emits WHATWG-conform event frames
// (https://html.spec.whatwg.org/multipage/server-sent-events.html) with one
// flush per event so bytes reach the client as they arrive, and applies the
// HTTP/2 comment trick: WriteHeader is followed by a single spec-ignored
// `: ok` comment frame — the comment produces a DATA frame that releases the
// HEADERS frame HTTP/2-aware reverse proxies otherwise hold back.
package sse

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Writer writes SSE events to a client connection, flushing per frame.
//
// A Writer wraps either an http.ResponseWriter (WriteHeader + automatic
// header comment) or a bare io.Writer (WriteHeader is a no-op; the header
// comment still precedes the first frame so HTTP/2-proxy-fronted relays of
// piped streams behave identically).
type Writer struct {
	w       io.Writer
	flusher http.Flusher
	header  bool // header comment emitted
}

// NewWriter returns a Writer for an HTTP response sink or a bare writer.
func NewWriter(w io.Writer) *Writer {
	sw := &Writer{w: w}
	if f, ok := w.(http.Flusher); ok {
		sw.flusher = f
	}
	return sw
}

// WriteHeader delegates to the sink's WriteHeader and emits the header
// comment (`: ok`), flushing once — releasing the HTTP/2 HEADERS frame
// behind proxies before any event is due. Without an http.ResponseWriter
// sink only the comment frame is written.
//
// The event-stream response headers are set here, immediately before the
// status: an SSE response MUST carry Content-Type: text/event-stream. A
// browser's native EventSource FAILS the connection on any other type
// (WHATWG SSE), so an untyped response silently degrades every live feed
// into a dead one — and streaming API clients are entitled to reject it
// too. no-cache keeps intermediaries from buffering frames, and
// X-Accel-Buffering disables the same buffering on nginx-fronted
// deployments (the header is ignored where it does not apply).
func (w *Writer) WriteHeader(status int) {
	if rw, ok := w.w.(http.ResponseWriter); ok {
		h := rw.Header()
		if h.Get("Content-Type") == "" {
			h.Set("Content-Type", "text/event-stream")
		}
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		rw.WriteHeader(status)
	}
	w.headerComment()
}

// headerComment emits the `: ok` comment frame exactly once; explicit
// frames before WriteHeader still see it first.
func (w *Writer) headerComment() {
	if w.header {
		return
	}
	w.header = true
	// A failing header comment must not break the stream; the first real
	// frame's error propagates instead (a write error means the client
	// disconnected).
	_ = w.write(": ok\n\n")
}

// Event writes one named event: `event:` + `data:` lines and the dispatching
// blank line, flushing after the frame.
func (w *Writer) Event(name, data string) error {
	return w.frame(fmt.Sprintf("event: %s\n", name), data)
}

// Data writes one unnamed (data-only) event — the OpenAI chat surface form.
func (w *Writer) Data(data string) error {
	return w.frame("", data)
}

// Comment writes a `:` comment line — spec-ignored by clients, but producing
// a relayed DATA frame that resets the client's silence watchdog. One flush
// per comment. Non-empty text separates with one space (the reader strips
// exactly one); a bare comment emits the minimal `:` line.
func (w *Writer) Comment(text string) error {
	if text != "" {
		text = " " + text
	}
	return w.write(":" + text + "\n\n")
}

// Raw writes a verbatim wire frame (e.g. the OpenAI `data: [DONE]`
// terminator) and flushes.
func (w *Writer) Raw(frame string) error {
	return w.write(frame)
}

// frame assembles an event frame: the event line (when named), the data
// payload split into one `data:` line per embedded newline (a raw newline
// must never leak into the wire as a forged field line; the reader rejoins
// the lines with one LF per field), and the dispatch blank line.
func (w *Writer) frame(eventLine, data string) error {
	if !w.header {
		w.headerComment()
	}
	var b strings.Builder
	b.WriteString(eventLine)
	for _, line := range strings.Split(data, "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return w.write(b.String())
}

// write emits one complete frame and flushes.
func (w *Writer) write(frame string) error {
	if _, err := io.WriteString(w.w, frame); err != nil {
		return fmt.Errorf("sse: write frame: %w", err)
	}
	if w.flusher != nil {
		w.flusher.Flush()
	}
	return nil
}
