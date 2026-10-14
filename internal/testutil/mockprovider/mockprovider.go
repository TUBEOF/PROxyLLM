// Package mockprovider provides a scripted mock upstream speaking the
// OpenAI-compatible chat-completions wire over real HTTP (the same wire
// custom.New consumes), so client → proxy → upstream round trips exercise
// the genuine provider stack: httpx transport, SSE decoding, error mapping,
// cancellation, and bounded-channel backpressure.
package mockprovider

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/provider/custom"
)

// RecordedRequest captures what the upstream received, for assertions on
// auth headers, paths, and the encoded wire body.
type RecordedRequest struct {
	Path   string
	Header http.Header
	Body   string
	Stream bool
}

// frame is one streamed write action.
type frame struct {
	kind string // "data", "comment", "named", "raw", "delay", "abort"
	name string // named events: the event field value
	data string // data/comment payload or raw block
	d    time.Duration
}

// Script describes one upstream response: either a full HTTP respond
// (JSONResponse/Fail) or a stream of frames (Chunk/Comment/NamedEvent/
// Delay/Abort/SSEFrames). Scripts are queued FIFO — one per request.
type Script struct {
	mode    int // modeRespond or modeStream; 0 until the first builder call
	status  int
	headers map[string]string
	body    string
	frames  []frame
}

const (
	modeRespond = 1
	modeStream  = 2
)

// NewScript returns an empty script.
func NewScript() *Script { return &Script{} }

// JSONResponse makes the script a non-streaming HTTP response with the
// given status, body, and extra headers (Content-Type: application/json
// unless overridden).
func (s *Script) JSONResponse(status int, body string, headers map[string]string) *Script {
	s.mode = modeRespond
	s.status = status
	s.body = body
	s.headers = map[string]string{"Content-Type": "application/json"}
	for k, v := range headers {
		s.headers[k] = v
	}
	return s
}

// Fail makes the script a non-2xx response carrying an OpenAI-shaped error
// body, optionally with a Retry-After header.
func (s *Script) Fail(status int, body string, retryAfter string) *Script {
	var headers map[string]string
	if retryAfter != "" {
		headers = map[string]string{"Retry-After": retryAfter}
	}
	return s.JSONResponse(status, body, headers)
}

// SSEFrames makes the script a stream serving the given raw SSE bytes
// (already data:-prefixed, e.g. a golden .sse fixture), flushed block by
// block.
func (s *Script) SSEFrames(raw string) *Script {
	s.mode = modeStream
	s.frames = append(s.frames, frame{kind: "raw", data: raw})
	return s
}

// Chunk appends one data-only frame carrying a chat.completion.chunk (or
// the [DONE] sentinel).
func (s *Script) Chunk(data string) *Script {
	s.mode = modeStream
	s.frames = append(s.frames, frame{kind: "data", data: data})
	return s
}

// Comment appends an SSE comment frame (keep-alive).
func (s *Script) Comment(text string) *Script {
	s.mode = modeStream
	s.frames = append(s.frames, frame{kind: "comment", data: text})
	return s
}

// NamedEvent appends a named event frame — the shape a future protocol
// version might add and current decoders must tolerate without breaking.
func (s *Script) NamedEvent(name, data string) *Script {
	s.mode = modeStream
	s.frames = append(s.frames, frame{kind: "named", name: name, data: data})
	return s
}

// Delay appends a pause between frames, interrupted by request-context
// cancellation (so a canceled consumer never wedges the test server).
func (s *Script) Delay(d time.Duration) *Script {
	s.mode = modeStream
	s.frames = append(s.frames, frame{kind: "delay", d: d})
	return s
}

// Abort appends a hard connection drop (panic(http.ErrAbortHandler)) — the
// mid-stream transport failure the failover discipline must surface as a
// terminal ir.Error event.
func (s *Script) Abort() *Script {
	s.mode = modeStream
	s.frames = append(s.frames, frame{kind: "abort"})
	return s
}

// Server is a scripted OpenAI-compatible upstream. Scripts are consumed
// FIFO, one per incoming request.
type Server struct {
	srv *httptest.Server

	mu    sync.Mutex
	queue []*Script
	reqs  []RecordedRequest
}

// New starts a mock upstream server; it is closed automatically at test
// cleanup.
func New(t *testing.T) *Server {
	s := &Server{}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

// URL returns the upstream base URL.
func (s *Server) URL() string { return s.srv.URL }

// Enqueue appends scripts to the FIFO queue, in order.
func (s *Server) Enqueue(scripts ...*Script) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, scripts...)
}

// Requests returns all requests received so far, in arrival order.
func (s *Server) Requests() []RecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RecordedRequest, len(s.reqs))
	copy(out, s.reqs)
	return out
}

// Close shuts the upstream down immediately.
func (s *Server) Close() { s.srv.Close() }

// Provider returns a real custom provider instance pointed at this mock —
// the same provider class operators configure for OpenAI-compatible
// endpoints, so tests exercise the production stack end to end.
func (s *Server) Provider(id string, caps provider.Capabilities) provider.Provider {
	return custom.New(id, s.srv.URL, "mock-key", caps)
}

// next pops the next script. The lock is held only briefly; the script is
// consumed exclusively by the calling handler.
func (s *Server) next() *Script {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return nil
	}
	sc := s.queue[0]
	s.queue = s.queue[1:]
	return sc
}

func (s *Server) record(r RecordedRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, r)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "mockprovider: unreadable request body", http.StatusBadRequest)
		return
	}
	var probe struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		http.Error(w, "mockprovider: undecodable request body", http.StatusBadRequest)
		return
	}
	s.record(RecordedRequest{
		Path:   r.URL.Path,
		Header: r.Header.Clone(),
		Body:   string(body),
		Stream: probe.Stream,
	})

	sc := s.next()
	if sc == nil {
		http.Error(w, `{"error":{"message":"mockprovider: script queue empty","type":"mock_error"}}`,
			http.StatusInternalServerError)
		return
	}

	switch sc.mode {
	case modeRespond:
		for k, v := range sc.headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(sc.status)
		_, _ = w.Write([]byte(sc.body))
	case modeStream:
		s.serveStream(w, r, sc)
	default:
		http.Error(w, `{"error":{"message":"mockprovider: empty script","type":"mock_error"}}`,
			http.StatusInternalServerError)
	}
}

// serveStream relays the scripted frames, flushing after each one so the
// client observes genuine incremental arrival (pacing), and honoring
// request-context cancellation for delays.
func (s *Server) serveStream(w http.ResponseWriter, r *http.Request, sc *Script) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	write := func(b string) {
		_, _ = w.Write([]byte(b))
		if flusher != nil {
			flusher.Flush()
		}
	}

	for _, f := range sc.frames {
		switch f.kind {
		case "data":
			write("data: " + f.data + "\n\n")
		case "comment":
			write(": " + f.data + "\n\n")
		case "named":
			write("event: " + f.name + "\ndata: " + f.data + "\n\n")
		case "raw":
			for _, block := range strings.Split(f.data, "\n\n") {
				write(block + "\n\n")
			}
		case "delay":
			select {
			case <-time.After(f.d):
			case <-r.Context().Done():
				return
			}
		case "abort":
			// Drop the connection mid-stream; httptest recovers the panic
			// and closes the TCP connection under the client.
			panic(http.ErrAbortHandler)
		}
	}
}
