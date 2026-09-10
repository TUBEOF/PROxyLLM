package ollama

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/proxyllm/proxyllm/internal/translate"
	"github.com/proxyllm/proxyllm/internal/translate/ir"
)

// streamDecoder decodes the Ollama /api/chat NDJSON stream (L6 pinned via
// the golden fixture, OP-4) into canonical IR events.
//
// Each line is a standalone JSON object with a partial message. The final
// line carries done:true, done_reason, and the usage counts. Block
// transitions are implicit — unlike Anthropic there are no block framing
// events — so the machine tracks the currently open block kind and emits
// BlockStop/BlockStart when the payload kind (text, thinking, tool_calls)
// changes:
//
//	empty first chunk → Start + BlockStart(text)
//	content fragments → Delta(text)
//	thinking appears → BlockStop(text) + BlockStart(thinking) + Delta
//	tool_calls appear → BlockStop + BlockStart(tool_use) + Delta(input)
//	done:true → BlockStop + MessageDelta(stop_reason, usage) + Done
type streamDecoder struct {
	br *bufio.Reader
	// queue holds canonical events decoded but not yet returned (one line
	// can map to several events).
	queue []ir.StreamEvent
	// openKind is the payload kind of the currently open block:
	// "", "text", "thinking", "tool". openIndex is its IR index.
	openKind  string
	openIndex int
	blockCnt  int
	started   bool
	done      bool
}

func newStreamDecoder(r io.Reader) translate.StreamDecoder {
	return &streamDecoder{br: bufio.NewReader(r)}
}

// Read returns the next canonical event. It returns io.EOF after Done (or
// a terminal error) and at clean stream end.
func (d *streamDecoder) Read(_ context.Context) (ir.StreamEvent, error) {
	return d.next()
}

// next pops queued events; when the queue is empty it reads NDJSON lines
// until one produces canonical output or the stream ends.
func (d *streamDecoder) next() (ir.StreamEvent, error) {
	for {
		if len(d.queue) > 0 {
			ev := d.queue[0]
			d.queue = d.queue[1:]
			return ev, nil
		}
		if d.done {
			return ir.StreamEvent{}, io.EOF
		}
		line, err := d.br.ReadBytes('\n')
		if len(line) == 0 && err == nil {
			continue // blank line, no events queued
		}
		if len(line) == 0 {
			return ir.StreamEvent{}, io.EOF // includes err == io.EOF
		}
		if err != nil && err != io.EOF {
			return ir.StreamEvent{}, fmt.Errorf("ollama: stream read: %w", err)
		}
		var w wireResponse
		if err := json.Unmarshal(line, &w); err != nil {
			return ir.StreamEvent{}, fmt.Errorf("ollama: decode stream line: %w", err)
		}
		d.mapChunk(&w)
	}
}

// mapChunk appends the canonical events for one NDJSON line to the queue.
func (d *streamDecoder) mapChunk(w *wireResponse) {
	if !d.started {
		d.started = true
		d.queue = append(d.queue,
			ir.StreamEvent{Type: ir.Start, Response: &ir.Response{Model: w.Model}},
		)
	}
	if w.Message.Content != "" {
		d.openBlock("text", ir.TextPart{})
		d.queue = append(d.queue, ir.StreamEvent{Type: ir.Delta, Index: d.openIndex,
			Delta: ir.DeltaPayload{Kind: ir.DeltaText, Text: w.Message.Content}})
	}
	if w.Message.Thinking != "" {
		d.openBlock("thinking", ir.ThinkingPart{})
		d.queue = append(d.queue, ir.StreamEvent{Type: ir.Delta, Index: d.openIndex,
			Delta: ir.DeltaPayload{Kind: ir.DeltaThinking, Text: w.Message.Thinking}})
	}
	for _, wtc := range w.Message.ToolCalls {
		d.openBlock("tool", ir.ToolCallPart{Name: wtc.Function.Name})
		// Ollama delivers tool arguments as complete JSON objects (L10);
		// they arrive as one fragment, not assembled wire deltas.
		d.queue = append(d.queue, ir.StreamEvent{Type: ir.Delta, Index: d.openIndex,
			Delta: ir.DeltaPayload{Kind: ir.DeltaToolInput, PartialJSON: string(wtc.Function.Arguments)}})
	}
	if w.Done {
		d.closeBlock()
		d.queue = append(d.queue,
			ir.StreamEvent{Type: ir.MessageDelta, MessageDelta: &ir.MessageDeltaPayload{
				StopReason: stopReasonFromWire(w.DoneReason),
				Usage:      usageFromWire(*w),
			}},
			ir.StreamEvent{Type: ir.Done},
		)
		d.done = true
	}
}

// openBlock ensures a block of the given kind is open, emitting BlockStop +
// BlockStart transitions when the payload kind changed (implicit framing).
func (d *streamDecoder) openBlock(kind string, part ir.Part) {
	if d.openKind != kind {
		d.closeBlock()
		d.openKind = kind
		d.openIndex = d.blockCnt
		d.blockCnt++
		d.queue = append(d.queue, ir.StreamEvent{Type: ir.BlockStart, Index: d.openIndex, Part: part})
	}
}

func (d *streamDecoder) closeBlock() {
	if d.openKind != "" {
		d.queue = append(d.queue, ir.StreamEvent{Type: ir.BlockStop, Index: d.openIndex})
		d.openKind = ""
	}
}
