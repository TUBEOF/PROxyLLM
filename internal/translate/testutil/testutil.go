// Package testutil provides shared golden-fixture helpers for codec contract
// tests (ADR-006). It is a normal (non _test) package so
// every protocol suite can import it; nothing outside tests may use it.
//
// All helpers resolve fixture paths relative to testdata/golden/ at the
// repository root. Tests derive expectations exclusively from the fixtures,
// never from codec behavior.
package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// goldenRoot resolves the repository's golden fixture directory by walking
// up from the test's working directory until testdata/golden is found. Codec
// and provider suites sit at different nesting depths, so a fixed relative
// hop would break one or the other; discovery keeps a single loader for all
// suites. Falls back to the codec-relative path when discovery fails.
var (
	goldenRootOnce sync.Once
	goldenRootPath string
)

func goldenRoot() string {
	goldenRootOnce.Do(func() {
		goldenRootPath = filepath.Join("..", "..", "..", "testdata", "golden")
		dir, err := os.Getwd()
		if err != nil {
			return
		}
		for d := dir; ; d = filepath.Dir(d) {
			cand := filepath.Join(d, "testdata", "golden")
			if st, err := os.Stat(cand); err == nil && st.IsDir() {
				goldenRootPath = cand
				return
			}
			if parent := filepath.Dir(d); parent == d {
				return
			}
		}
	})
	return goldenRootPath
}

// wireSearchItem mirrors one web_search_result entry of a "search" array in
// .expected.json fixtures (the result block arrives COMPLETE in
// content_block_start — stream fixtures carry the items up front).
type wireSearchItem struct {
	URL              string `json:"url"`
	Title            string `json:"title"`
	EncryptedContent string `json:"encrypted_content,omitempty"`
	PageAge          string `json:"page_age,omitempty"`
}

// wirePart mirrors the "part" object of .expected.json fixtures: a kind
// discriminator plus the fields the fixtures carry.
type wirePart struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`

	// Server-tool result fields (kind "server_tool_result").
	ToolUseID string           `json:"tool_use_id,omitempty"`
	ToolName  string           `json:"tool_name,omitempty"`
	IsError   bool             `json:"is_error,omitempty"`
	ErrorCode string           `json:"error_code,omitempty"`
	Search    []wireSearchItem `json:"search,omitempty"`
}

// wireError mirrors the "error" object of .expected.json fixtures.
type wireError struct {
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// wireEvent is the JSON shape of a canonical event in .expected.json.
type wireEvent struct {
	Type         string                  `json:"type"`
	Index        int                     `json:"index,omitempty"`
	Model        string                  `json:"model,omitempty"`
	ID           string                  `json:"id,omitempty"`
	Part         *wirePart               `json:"part,omitempty"`
	Delta        *ir.DeltaPayload        `json:"delta,omitempty"`
	MessageDelta *ir.MessageDeltaPayload `json:"message_delta,omitempty"`
	Usage        *ir.Usage               `json:"usage,omitempty"`
	Error        *wireError              `json:"error,omitempty"`
}

// LoadGolden reads a golden fixture relative to testdata/golden/ and
// returns its raw bytes. Fails the test when the file cannot be read.
func LoadGolden(t *testing.T, rel string) []byte {
	t.Helper()
	// #nosec G304 -- rel is a test-supplied fixture path (constant names in
	// contract tests), never operator or request input.
	raw, err := os.ReadFile(filepath.Join(goldenRoot(), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("load golden fixture %s: %v", rel, err)
	}
	return raw
}

// LoadExpectedEvents reads a .expected.json canonical event sequence
// relative to testdata/golden/ and decodes it into canonical events.
func LoadExpectedEvents(t *testing.T, rel string) []ir.StreamEvent {
	t.Helper()
	raw := LoadGolden(t, rel)
	var wire []wireEvent
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode expected events %s: %v", rel, err)
	}
	out := make([]ir.StreamEvent, 0, len(wire))
	for i, we := range wire {
		ev := ir.StreamEvent{Type: ir.StreamEventType(we.Type), Index: we.Index}
		switch {
		case we.Type == "start":
			ev.Response = &ir.Response{
				ID:    we.ID,
				Model: we.Model,
				Usage: derefUsage(we.Usage),
			}
		case we.Part != nil:
			ev.Part = partFromWire(we.Part)
		case we.Delta != nil:
			ev.Delta = *we.Delta
		case we.MessageDelta != nil:
			ev.MessageDelta = we.MessageDelta
		case we.Error != nil:
			ev.Error = &ir.UpstreamError{
				Type:    we.Error.Type,
				Code:    we.Error.Code,
				Message: we.Error.Message,
			}
		}
		if ev.Type == ir.Error && ev.Error == nil {
			t.Fatalf("%s: event %d is type error but carries no error object", rel, i)
		}
		out = append(out, ev)
	}
	return out
}

func derefUsage(u *ir.Usage) ir.Usage {
	if u == nil {
		return ir.Usage{}
	}
	return *u
}

func partFromWire(p *wirePart) ir.Part {
	switch p.Kind {
	case "text":
		return ir.TextPart{}
	case "thinking":
		return ir.ThinkingPart{}
	case "tool_use":
		return ir.ToolCallPart{ID: p.ID, Name: p.Name}
	case "server_tool_use":
		// Stream fixtures never carry input at block_start — the input
		// arrives via input_json_delta (verified live 2026-09-12).
		return ir.ServerToolUsePart{ID: p.ID, Name: p.Name}
	case "server_tool_result":
		res := ir.ServerToolResultPart{
			ToolUseID: p.ToolUseID,
			ToolName:  p.ToolName,
			IsError:   p.IsError,
			ErrorCode: p.ErrorCode,
		}
		for _, it := range p.Search {
			res.Search = append(res.Search, ir.WebSearchResultItem{
				URL:              it.URL,
				Title:            it.Title,
				EncryptedContent: it.EncryptedContent,
				PageAge:          it.PageAge,
			})
		}
		return res
	default:
		return nil
	}
}

func partKind(p ir.Part) string {
	switch p.(type) {
	case ir.TextPart:
		return "text"
	case ir.ThinkingPart:
		return "thinking"
	case ir.ToolCallPart:
		return "tool_use"
	case ir.ImagePart:
		return "image"
	case ir.ToolResultPart:
		return "tool_result"
	case ir.ServerToolUsePart:
		return "server_tool_use"
	case ir.ServerToolResultPart:
		return "server_tool_result"
	default:
		return fmt.Sprintf("%T", p)
	}
}

// DrainStreamDecoder reads events from dec until io.EOF (or a non-EOF
// error) and returns them. An ir.Error event read before EOF is included
// in the returned slice, matching the StreamDecoder contract (terminal
// error event, then EOF).
func DrainStreamDecoder(t *testing.T, dec translateStreamDecoder) []ir.StreamEvent {
	t.Helper()
	var events []ir.StreamEvent
	for {
		ev, err := dec.Read(context.Background())
		if err == io.EOF {
			return events
		}
		if err != nil {
			t.Fatalf("stream decoder Read: %v", err)
		}
		events = append(events, ev)
		if ev.Type == ir.Error {
			// Terminal: continue draining so the decoder's following
			// EOF is observed (and any post-error event is a defect).
			continue
		}
	}
}

// translateStreamDecoder mirrors translate.StreamDecoder without
// importing the parent package (avoiding an import cycle in tests that
// use concrete decoder types).
type translateStreamDecoder interface {
	Read(ctx context.Context) (ir.StreamEvent, error)
}

// AssertEventsEqual compares a decoded canonical event sequence against a
// fixture-derived expectation. rel names the fixture for failure messages.
func AssertEventsEqual(t *testing.T, rel string, want, got []ir.StreamEvent) {
	t.Helper()
	max := len(want)
	if len(got) > max {
		max = len(got)
	}
	for i := 0; i < max; i++ {
		if i >= len(want) {
			t.Errorf("%s: event %d: unexpected extra event %+v", rel, i, got[i])
			continue
		}
		if i >= len(got) {
			t.Errorf("%s: event %d: missing event, want %+v", rel, i, want[i])
			continue
		}
		assertEventEqual(t, rel, i, want[i], got[i])
	}
	if len(want) != len(got) {
		t.Errorf("%s: event count = %d, want %d", rel, len(got), len(want))
	}
}

func assertEventEqual(t *testing.T, rel string, i int, want, got ir.StreamEvent) {
	t.Helper()
	if want.Type != got.Type {
		t.Errorf("%s: event %d: type = %q, want %q", rel, i, got.Type, want.Type)
		return
	}
	if want.Index != got.Index {
		t.Errorf("%s: event %d (%s): index = %d, want %d", rel, i, want.Type, got.Index, want.Index)
	}
	switch want.Type {
	case ir.Start:
		assertResponseEqual(t, rel, i, want.Response, got.Response)
	case ir.BlockStart:
		assertPartEqual(t, rel, i, want.Part, got.Part)
	case ir.Delta:
		if want.Delta != got.Delta {
			t.Errorf("%s: event %d (delta): delta = %+v, want %+v", rel, i, got.Delta, want.Delta)
		}
	case ir.MessageDelta:
		if want.MessageDelta == nil || got.MessageDelta == nil {
			t.Errorf("%s: event %d (message_delta): payload nil (want %v, got %v)", rel, i, want.MessageDelta != nil, got.MessageDelta != nil)
			return
		}
		if want.MessageDelta.StopReason != got.MessageDelta.StopReason {
			t.Errorf("%s: event %d: stop_reason = %q, want %q", rel, i, got.MessageDelta.StopReason, want.MessageDelta.StopReason)
		}
		assertUsageEqual(t, rel, i, want.MessageDelta.Usage, got.MessageDelta.Usage)
	case ir.Error:
		if want.Error == nil || got.Error == nil {
			t.Errorf("%s: event %d (error): payload nil (want %v, got %v)", rel, i, want.Error != nil, got.Error != nil)
			return
		}
		if want.Error.Type != got.Error.Type || want.Error.Code != got.Error.Code || want.Error.Message != got.Error.Message {
			t.Errorf("%s: event %d: error = %+v, want %+v", rel, i, got.Error, want.Error)
		}
	}
}

func assertResponseEqual(t *testing.T, rel string, i int, want, got *ir.Response) {
	t.Helper()
	if (want == nil) != (got == nil) {
		t.Errorf("%s: event %d (start): response snapshot nil mismatch (want %v, got %v)", rel, i, want != nil, got != nil)
		return
	}
	if want == nil {
		return
	}
	if want.ID != got.ID || want.Model != got.Model {
		t.Errorf("%s: event %d (start): id/model = %q/%q, want %q/%q", rel, i, got.ID, got.Model, want.ID, want.Model)
	}
	assertUsageEqual(t, rel, i, want.Usage, got.Usage)
}

func assertPartEqual(t *testing.T, rel string, i int, want, got ir.Part) {
	t.Helper()
	if (want == nil) != (got == nil) {
		t.Errorf("%s: event %d (block_start): part nil mismatch (want %v, got %v)", rel, i, want != nil, got != nil)
		return
	}
	if want == nil {
		return
	}
	wk, gk := partKind(want), partKind(got)
	if wk != gk {
		t.Errorf("%s: event %d (block_start): part kind = %q, want %q", rel, i, gk, wk)
		return
	}
	if wtc, ok := want.(ir.ToolCallPart); ok {
		gtc, ok := got.(ir.ToolCallPart)
		if !ok {
			t.Errorf("%s: event %d (block_start): part = %T, want ir.ToolCallPart", rel, i, got)
			return
		}
		if wtc.ID != gtc.ID || wtc.Name != gtc.Name {
			t.Errorf("%s: event %d (block_start): tool_use id/name = %q/%q, want %q/%q", rel, i, gtc.ID, gtc.Name, wtc.ID, wtc.Name)
		}
	}
	if wst, ok := want.(ir.ServerToolUsePart); ok {
		gst, ok := got.(ir.ServerToolUsePart)
		if !ok {
			t.Errorf("%s: event %d (block_start): part = %T, want ir.ServerToolUsePart", rel, i, got)
			return
		}
		if wst.ID != gst.ID || wst.Name != gst.Name {
			t.Errorf("%s: event %d (block_start): server_tool_use id/name = %q/%q, want %q/%q", rel, i, gst.ID, gst.Name, wst.ID, wst.Name)
		}
	}
	if wsr, ok := want.(ir.ServerToolResultPart); ok {
		gsr, ok := got.(ir.ServerToolResultPart)
		if !ok {
			t.Errorf("%s: event %d (block_start): part = %T, want ir.ServerToolResultPart", rel, i, got)
			return
		}
		if wsr.ToolUseID != gsr.ToolUseID || wsr.ToolName != gsr.ToolName ||
			wsr.IsError != gsr.IsError || wsr.ErrorCode != gsr.ErrorCode {
			t.Errorf("%s: event %d (block_start): server_tool_result = %+v, want %+v", rel, i, gsr, wsr)
		}
		if !reflect.DeepEqual(wsr.Search, gsr.Search) {
			t.Errorf("%s: event %d (block_start): search items = %+v, want %+v", rel, i, gsr.Search, wsr.Search)
		}
	}
}

func assertUsageEqual(t *testing.T, rel string, i int, want, got ir.Usage) {
	t.Helper()
	if !reflect.DeepEqual(want, got) {
		t.Errorf("%s: event %d: usage = %+v, want %+v", rel, i, got, want)
	}
}
