// Package sse implements a WHATWG-compliant Server-Sent Events reader
// shared by the SSE-speaking codecs (anthropic, openai).
//
// Rules implemented (WHATWG SSE specification, facts SSE-1..SSE-7,
// research digest 2026-09-10):
//   - Lines terminate on CRLF, LF, or lone CR (SSE-1).
//   - A leading UTF-8 BOM on the stream is stripped (SSE-2).
//   - Lines starting with ":" are comments — surfaced as comment events
//     so codecs can relay them (SSE-3, keepalive C2 / H2-2), without
//     disturbing the pending event (SSE-5).
//   - Fields split at the first colon; one leading space of the value is
//     stripped (SSE-4).
//   - A blank line dispatches the pending event; an empty data buffer
//     dispatches nothing (SSE-5).
//   - Multiple data lines concatenate with "\n" between them (SSE-7).
//   - A pending, undispatched event at EOF is discarded (SSE-6).
package sse

import (
	"bufio"
	"bytes"
	"io"
	"strings"
)

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// Event is one dispatched SSE event or a comment line.
type Event struct {
	// Name is the "event" field value; "" when absent.
	Name string

	// Data is the concatenated data buffer (SSE-7).
	Data string

	// Comment carries the text after ":" on a comment line (IsComment
	// events carry nothing else).
	Comment string

	// IsComment marks comment lines (SSE-3).
	IsComment bool
}

// Reader reads SSE events from a byte stream.
type Reader struct {
	br    *bufio.Reader
	first bool // first line pending BOM strip

	name    string
	data    []string
	hasData bool
}

// NewReader returns a Reader consuming r. Reading is incremental — events
// are yielded as soon as their terminating blank line arrives (SSE-7: no
// stream buffering beyond the pending event).
func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, 64*1024), first: true}
}

// Next returns the next comment or dispatched event. It returns io.EOF at
// clean stream end (pending event discarded per SSE-6); other errors are
// passed through from the underlying stream.
func (r *Reader) Next() (Event, error) {
	for {
		line, err := r.readLine()
		if err == io.EOF {
			// SSE-6: an event that never reached its dispatching blank
			// line is discarded, not dispatched.
			r.reset()
			return Event{}, io.EOF
		}
		if err != nil {
			return Event{}, err
		}
		if r.first {
			line = bytes.TrimPrefix(line, utf8BOM)
			r.first = false
		}
		if len(line) == 0 {
			// Blank line: dispatch (SSE-5).
			if r.hasData {
				ev := Event{Name: r.name, Data: strings.Join(r.data, "\n")}
				r.reset()
				if ev.Data != "" {
					return ev, nil
				}
			} else {
				r.reset()
			}
			continue
		}
		if line[0] == ':' {
			// Comment (SSE-3): ignored per spec, surfaced for relay.
			return Event{IsComment: true, Comment: string(line[1:])}, nil
		}
		field, value := splitField(line)
		switch field {
		case "event":
			r.name = value
		case "data":
			r.data = append(r.data, value)
			r.hasData = true
		default:
			// "id", "retry", and unknown fields are ignored (no
			// reconnection support in a proxy relay).
		}
	}
}

// reset clears the pending event state.
func (r *Reader) reset() {
	r.name = ""
	r.data = nil
	r.hasData = false
}

// splitField splits a field line at the first colon, stripping one leading
// space from the value (SSE-4).
func splitField(line []byte) (field, value string) {
	if i := bytes.IndexByte(line, ':'); i >= 0 {
		return string(line[:i]), strings.TrimPrefix(string(line[i+1:]), " ")
	}
	return string(line), ""
}

// readLine reads one line, accepting CRLF, LF, and lone CR as line
// terminators (SSE-1). At EOF it returns the partial line together with
// io.EOF; the caller discards it (SSE-6).
func (r *Reader) readLine() ([]byte, error) {
	var line []byte
	for {
		b, err := r.br.ReadByte()
		if err != nil {
			return line, err
		}
		switch b {
		case '\n':
			return line, nil
		case '\r':
			b2, err := r.br.ReadByte()
			if err == io.EOF {
				return line, nil // CR at EOF terminates the line
			}
			if err != nil {
				return line, err
			}
			if b2 != '\n' {
				if err := r.br.UnreadByte(); err != nil {
					return line, err
				}
			}
			return line, nil
		default:
			line = append(line, b)
		}
	}
}
