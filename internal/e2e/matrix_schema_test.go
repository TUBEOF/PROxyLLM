package e2e

// Schema validation (WP6): every pair cell validates the client-facing
// response bytes against the OFFICIAL wire schema of its surface, in addition
// to the hand-crafted per-cell assertions.
//
// The schemas live in testdata/schemas/ next to the surface's protocol, each
// with a .meta.json carrying the official source URL, the source version/commit
// and the access date — the same provenance contract as the golden fixtures.
// A schema that does not come from the vendor's own published definition is a
// defect, not a snapshot.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/dlclark/regexp2"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// wireRecord is one captured client-facing response body.
type wireRecord struct {
	method string
	path   string
	body   []byte
}

// wireCapture tees response bodies at the proxy boundary so a cell can
// validate the exact bytes the client's surface emitted — before any SDK
// parser got its hands on them.
type wireCapture struct {
	mu       sync.Mutex
	inFlight sync.WaitGroup
	recs     []wireRecord
}

func (wc *wireCapture) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wc.inFlight.Add(1)
		defer wc.inFlight.Done()
		cw := &captureWriter{ResponseWriter: w}
		next.ServeHTTP(cw, r)
		wc.mu.Lock()
		defer wc.mu.Unlock()
		// The WS transport upgrades and then hijacks the connection; the
		// hijacked frames never pass through Write, so an empty capture with
		// a switched protocol is expected and recorded as such.
		body := cw.buf.Bytes()
		if cw.hijacked {
			body = nil
		}
		wc.recs = append(wc.recs, wireRecord{
			method: r.Method,
			path:   r.URL.Path,
			body:   body,
		})
	})
}

// drain blocks until every in-flight record has been appended. A stream's
// handler may outlive the client's read by a tick: the validation must not
// race the capture.
func (wc *wireCapture) drain(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() { wc.inFlight.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		wc.mu.Lock()
		defer wc.mu.Unlock()
		t.Fatalf("wire capture still draining: %d records kept", len(wc.recs))
	}
}

// clientBody returns the one captured response for a method+path. Cells make
// exactly one client request, so "the single matching record" is the contract;
// anything else is a mapping bug, not a lookup miss.
func (wc *wireCapture) clientBody(t *testing.T, method, path string) wireRecord {
	t.Helper()
	wc.drain(t)
	wc.mu.Lock()
	defer wc.mu.Unlock()
	var hits []wireRecord
	for _, rec := range wc.recs {
		if rec.method == method && rec.path == path {
			hits = append(hits, rec)
		}
	}
	if len(hits) != 1 {
		var seen []string
		for _, rec := range wc.recs {
			seen = append(seen, rec.method+" "+rec.path)
		}
		t.Fatalf("captured responses matching %s %s = %d (all: %v)", method, path, len(hits), seen)
	}
	return hits[0]
}

// captureWriter doubles as HTTP response tee and full streaming/hijack
// passthrough: relay code asserts Flusher, and the Responses WebSocket
// transport hijacks.
type captureWriter struct {
	http.ResponseWriter
	buf      bytes.Buffer
	hijacked bool
}

func (c *captureWriter) Write(p []byte) (int, error) {
	c.buf.Write(p)
	return c.ResponseWriter.Write(p)
}

func (c *captureWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *captureWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c.hijacked = true
	h, ok := c.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

// ---- schema registry ----

var schemaCache sync.Map // file name → *jsonschema.Schema

// compileWireSchema compiles one testdata/schemas document. Compilation is
// shared across subtests: a broken schema fails every cell that uses it, once.
//
// The regexp engine is dlclark/regexp2 (ECMAScript mode), because several
// official OpenAI schema patterns use ECMA lookahead/lookbehind constructs
// that Go's stdlib regexp cannot parse.
func compileWireSchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	if v, ok := schemaCache.Load(name); ok {
		return v.(*jsonschema.Schema)
	}
	c := jsonschema.NewCompiler()
	c.UseRegexpEngine(dlclarkCompile)
	sch, err := c.Compile(filepath.Join("testdata", "schemas", name))
	if err != nil {
		t.Fatalf("compile schema %s: %v", name, err)
	}
	schemaCache.Store(name, sch)
	return sch
}

// dlclarkCompile adapts dlclark/regexp2 to the validator's Regexp interface.
type pcreRegexp struct {
	re *regexp2.Regexp
}

func (re *pcreRegexp) MatchString(s string) bool {
	matched, err := re.re.MatchString(s)
	return err == nil && matched
}

func (re *pcreRegexp) String() string {
	return re.re.String()
}

func dlclarkCompile(s string) (jsonschema.Regexp, error) {
	re, err := regexp2.Compile(s, regexp2.ECMAScript)
	if err != nil {
		return nil, err
	}
	return &pcreRegexp{re: re}, nil
}

// sseDataFrames splits one response body into its `data:` JSON payloads
// (per-event dispatch on the blank line, multiple data lines joined with \n —
// the framing both our SSE writer and the upstream simulators follow).
// The literal terminal ("[DONE]") yields nothing.
func sseDataFrames(body []byte) ([][]byte, error) {
	var frames [][]byte
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var data []string
	hasData := false
	dispatch := func() {
		if hasData {
			joined := strings.Join(data, "\n")
			if joined != "[DONE]" && strings.TrimSpace(joined) != "" {
				frames = append(frames, []byte(joined))
			}
		}
		data, hasData = nil, false
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			dispatch()
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			hasData = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	dispatch()
	return frames, nil
}

// jsonLines splits one NDJSON body into its frames.
func jsonLines(body []byte) ([][]byte, error) {
	var frames [][]byte
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			frames = append(frames, []byte(line))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return frames, nil
}

// singleFrame treats a JSON body as one frame.
func singleFrame(body []byte) ([][]byte, error) {
	if strings.TrimSpace(string(body)) == "" {
		return nil, nil
	}
	return [][]byte{body}, nil
}

// cellSurfaceSchema names the schema document and frame parser for one pair
// cell's client surface. The key is (client surface × streaming): a
// non-streaming response is one JSON document, a streaming one validates per
// emitted frame — the per-frame schema is the official one of that wire's
// stream units.
type cellSchema struct {
	file  string
	split func([]byte) ([][]byte, error)
	// kind labels a unit in failure output: "document", "chunk", "event",
	// "line", "frame".
	kind string
}

// pairSurface resolves (path, schema) for a pair cell's client surface.
func pairSurface(cell matrixCell) (path string, schema cellSchema) {
	stream := cell.Mode == "stream"
	switch cell.Client {
	case "openai-chat":
		if stream {
			return "/v1/chat/completions", cellSchema{"openai_chat_stream.json", sseDataFrames, "chunk"}
		}
		return "/v1/chat/completions", cellSchema{"openai_chat.json", singleFrame, "document"}
	case "openai-responses":
		if stream {
			return "/v1/responses", cellSchema{"openai_responses_stream.json", sseDataFrames, "event"}
		}
		return "/v1/responses", cellSchema{"openai_responses.json", singleFrame, "document"}
	case "anthropic-messages":
		if stream {
			return "/v1/messages", cellSchema{"anthropic_message_stream.json", sseDataFrames, "event"}
		}
		return "/v1/messages", cellSchema{"anthropic_message.json", singleFrame, "document"}
	case "ollama-native":
		// Streaming on the Ollama wire is NDJSON — one ChatStreamEvent per
		// line, the official application/x-ndjson response schema.
		if stream {
			return "/api/chat", cellSchema{"ollama_chat_stream.json", jsonLines, "line"}
		}
		return "/api/chat", cellSchema{"ollama_chat.json", singleFrame, "document"}
	case "gemini-native":
		action := "generateContent"
		if stream {
			action = "streamGenerateContent"
		}
		// Streaming frames are GenerateContentResponse like the document.
		if stream {
			return "/v1beta/models/" + cell.Upstream + ":" + action, cellSchema{"gemini_generate_content.json", sseDataFrames, "frame"}
		}
		return "/v1beta/models/" + cell.Upstream + ":" + action, cellSchema{"gemini_generate_content.json", singleFrame, "document"}
	default:
		return "", cellSchema{}
	}
}

// validatePairCellResponse schema-validates the client-facing bytes the proxy
// emitted for one pair cell. Failures print the named schema violations.
func validatePairCellResponse(t *testing.T, cell matrixCell, e *matrixEnv) {
	t.Helper()
	if cell.SchemaNote != "" {
		// A manifest-documented gap: the cell runs, the gap is re-stated on
		// every run, and no schema assertion is made. Never silent.
		t.Logf("%s: schema validation not yet strict — %s", cell.ID, cell.SchemaNote)
		return
	}
	path, schema := pairSurface(cell)
	if path == "" {
		t.Fatalf("no schema surface for client %q", cell.Client)
	}
	rec := e.wire.clientBody(t, http.MethodPost, path)
	if os.Getenv("PROXYLLM_E2E_SCHEMA_DUMP") != "" {
		t.Logf("%s (%s): %s", cell.ID, path, clipBytes(rec.body, 4096))
	}
	frames, err := schema.split(rec.body)
	if err != nil {
		t.Fatalf("%s: split %s body: %v", cell.ID, path, err)
	}
	if len(frames) == 0 {
		t.Fatalf("%s: no %s captured on %s — nothing to validate", cell.ID, schema.kind, path)
	}
	sch := compileWireSchema(t, schema.file)
	for i, frame := range frames {
		var doc any
		if err := json.Unmarshal(frame, &doc); err != nil {
			t.Errorf("%s: %s %d is not JSON: %v (%s)", cell.ID, schema.kind, i, err, clipBytes(frame, 300))
			continue
		}
		if err := sch.Validate(doc); err != nil {
			t.Errorf("%s: %s %d fails %s:\n%s", cell.ID, schema.kind, i, schema.file, validationErrorText(err))
		}
	}
}

// validationErrorText renders one validation result as named, per-unit errors
// (instance location + keyword + message). Union groups may have dozens of
// candidate causes; the first few are named and the rest counted, so a failing
// cell still carries the real violation in its output.
func validationErrorText(err error) string {
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return err.Error()
	}
	var b strings.Builder
	budget := totalWalkBudget
	for _, cause := range ve.Causes {
		if budget <= 0 {
			break
		}
		budget = walkErrors(&b, cause, 0, budget)
	}
	return b.String()
}

// maxGroupCauses names how many union candidates print per group;
// totalWalkBudget bounds the whole rendering.
const (
	maxGroupCauses  = 3
	totalWalkBudget = 14
)

// walkErrors prints one error node recursively; returns the remaining budget.
func walkErrors(b *strings.Builder, ve *jsonschema.ValidationError, depth, budget int) int {
	if depth > 8 || budget <= 0 {
		return budget
	}
	p := message.NewPrinter(language.English)
	b.WriteString(strings.Repeat("  ", depth))
	b.WriteString("/" + strings.Join(ve.InstanceLocation, "/"))
	b.WriteString(": /" + strings.Join(ve.ErrorKind.KeywordPath(), "/"))
	b.WriteString(" — ")
	b.WriteString(ve.ErrorKind.LocalizedString(p))
	b.WriteString("\n")
	budget--
	for i, cause := range ve.Causes {
		if budget <= 0 {
			return budget
		}
		if i >= maxGroupCauses {
			fmt.Fprintf(b, "%s… +%d more candidate shapes not shown\n", strings.Repeat("  ", depth+1), len(ve.Causes)-i)
			return budget
		}
		budget = walkErrors(b, cause, depth+1, budget)
	}
	return budget
}

// clipBytes shortens a raw frame for error output.
func clipBytes(p []byte, n int) string {
	s := string(p)
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}
