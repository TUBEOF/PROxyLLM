package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/proxyllm/proxyllm/internal/translate"
	"github.com/proxyllm/proxyllm/internal/translate/ir"
	"github.com/proxyllm/proxyllm/internal/translate/sse"
)

// streamDecoder decodes the OpenAI chat-completions SSE stream (facts O4,
// O12) into canonical IR events, using the shared WHATWG SSE reader.
//
// Chat streaming is data-only SSE terminated by the [DONE] sentinel. One
// wire chunk can map to zero, one, or two canonical events, so decoded
// events queue and Read hands them out one at a time. The machine:
//   - the first chunk (role assistant, typically empty content) → Start
//     (usage stays zero until the final usage chunk) + BlockStart(text)
//   - content deltas → Delta(text); empty content deltas skipped
//   - tool_calls entries assemble by index (O4): a new index closes the
//     open block and opens a tool_use block (ID/name on the first entry);
//     argument fragments → Delta(tool_input)
//   - the finish_reason chunk → BlockStop (stop reason recorded)
//   - a chunk carrying usage → MessageDelta with the recorded stop reason;
//     when no usage chunk arrives, MessageDelta is emitted from [DONE]
//     with zero usage
//   - [DONE] → Done; Read returns io.EOF afterwards
type streamDecoder struct {
	reader *sse.Reader

	// queue holds canonical events decoded but not yet returned.
	queue []ir.StreamEvent

	started    bool          // Start + initial text BlockStart emitted
	openBlock  int           // IR index of the open TEXT block; -1 = none
	blockCnt   int           // blocks opened so far (next index)
	toolBlocks map[int]int   // wire tool_calls index → IR index (open tool blocks)
	finish     ir.StopReason // recorded finish_reason ("" until seen)
	pendingMD  bool          // finish seen but MessageDelta not yet emitted
	done       bool          // [DONE] or terminal error decoded
}

func newStreamDecoder(r io.Reader) translate.StreamDecoder {
	return &streamDecoder{
		reader:    sse.NewReader(r),
		openBlock: -1,
	}
}

// Read returns the next canonical event. It returns io.EOF after [DONE] (or
// a terminal error event) has been handed out, and at clean stream end.
func (d *streamDecoder) Read(_ context.Context) (ir.StreamEvent, error) {
	if len(d.queue) > 0 {
		ev := d.queue[0]
		d.queue = d.queue[1:]
		return ev, nil
	}
	if d.done {
		return ir.StreamEvent{}, io.EOF
	}
	for {
		ev, err := d.reader.Next()
		if err == io.EOF {
			return ir.StreamEvent{}, io.EOF
		}
		if err != nil {
			return ir.StreamEvent{}, fmt.Errorf("openai: stream read: %w", err)
		}
		if ev.IsComment {
			return ir.StreamEvent{Type: ir.Comment}, nil
		}
		if ev.Data == "[DONE]" {
			if d.pendingMD {
				// No usage chunk arrived: emit the terminal MessageDelta
				// with zero usage, still before Done.
				d.queue = append(d.queue, d.terminalMessageDelta(ir.Usage{}))
				d.pendingMD = false
			}
			d.queue = append(d.queue, ir.StreamEvent{Type: ir.Done})
			d.done = true
			out := d.queue[0]
			d.queue = d.queue[1:]
			return out, nil
		}
		var w wireChunk
		if err := json.Unmarshal([]byte(ev.Data), &w); err != nil {
			return ir.StreamEvent{}, fmt.Errorf("openai: decode stream chunk: %w", err)
		}
		d.mapChunk(&w)
		if len(d.queue) > 0 {
			out := d.queue[0]
			d.queue = d.queue[1:]
			return out, nil
		}
		// The chunk carried nothing canonical; read the next one.
	}
}

// wireChunk mirrors the chat.completion.chunk JSON shape.
type wireChunk struct {
	ID      string        `json:"id"`
	Model   string        `json:"model"`
	Choices []wireChoice  `json:"choices"`
	Usage   *wireUsage    `json:"usage,omitempty"`
	Error   *wireAPIError `json:"error,omitempty"`
}

func (d *streamDecoder) terminalMessageDelta(u ir.Usage) ir.StreamEvent {
	return ir.StreamEvent{Type: ir.MessageDelta, MessageDelta: &ir.MessageDeltaPayload{
		StopReason: d.finish,
		Usage:      u,
	}}
}

// mapChunk appends the canonical events for one wire chunk to the queue.
func (d *streamDecoder) mapChunk(w *wireChunk) {
	if w.Error != nil {
		d.queue = append(d.queue, ir.StreamEvent{Type: ir.Error, Error: &ir.UpstreamError{
			Type:    w.Error.Type,
			Message: w.Error.Message,
		}})
		d.done = true
		return
	}
	if !d.started {
		// First chunk: Start (usage zero until the final usage chunk) +
		// BlockStart(text). Content in the same chunk is handled below.
		d.started = true
		d.openBlock = 0
		d.blockCnt = 1
		d.queue = append(d.queue,
			ir.StreamEvent{Type: ir.Start, Response: &ir.Response{ID: w.ID, Model: w.Model}},
			ir.StreamEvent{Type: ir.BlockStart, Index: 0, Part: ir.TextPart{}})
	}
	if len(w.Choices) == 0 {
		d.emitUsage(w.Usage)
		return
	}
	ch := w.Choices[0]
	if ch.Delta != nil && ch.Delta.Content.Str != nil && *ch.Delta.Content.Str != "" {
		// Content delta; only while a text block is open (a tool block
		// having been opened means the text block was closed).
		if d.openBlock >= 0 && len(d.toolBlocks) == 0 {
			d.queue = append(d.queue, ir.StreamEvent{Type: ir.Delta, Index: d.openBlock,
				Delta: ir.DeltaPayload{Kind: ir.DeltaText, Text: *ch.Delta.Content.Str}})
		}
	} else if ch.Delta != nil && len(ch.Delta.ToolCalls) > 0 {
		// Parallel tool calls (O4): one chunk can carry several
		// tool_calls entries, each assembling by its OWN index — loop
		// over all entries. Blocks stay open until finish_reason: a
		// later chunk can resume any index (interleaved fragments), so
		// toolBlocks maps wire index → IR index instead of tracking a
		// single open call.
		for _, tc := range ch.Delta.ToolCalls {
			if d.toolBlocks == nil {
				d.toolBlocks = make(map[int]int)
			}
			block, open := d.toolBlocks[tc.Index]
			if !open {
				// First entry for this index: close the open text
				// block, open a tool_use block.
				if d.openBlock >= 0 {
					d.queue = append(d.queue, ir.StreamEvent{Type: ir.BlockStop, Index: d.openBlock})
					d.openBlock = -1
				}
				block = d.blockCnt
				d.blockCnt++
				d.toolBlocks[tc.Index] = block
				d.queue = append(d.queue, ir.StreamEvent{Type: ir.BlockStart, Index: block,
					Part: ir.ToolCallPart{ID: tc.ID, Name: tc.Function.Name}})
			} else if tc.Function.Arguments != "" {
				// Argument fragment for this index's tool block.
				d.queue = append(d.queue, ir.StreamEvent{Type: ir.Delta, Index: block,
					Delta: ir.DeltaPayload{Kind: ir.DeltaToolInput, PartialJSON: tc.Function.Arguments}})
			}
		}
	}
	if ch.FinishReason != "" {
		d.finish = stopReasonFromWire(ch.FinishReason)
		d.pendingMD = true
		// Close the text block and every open tool block, in open
		// order: text first (its BlockStop already ran when the first
		// tool opened, so only emit if still open), then the tool
		// blocks in ascending IR index.
		if d.openBlock >= 0 {
			d.queue = append(d.queue, ir.StreamEvent{Type: ir.BlockStop, Index: d.openBlock})
			d.openBlock = -1
		}
		indices := make([]int, 0, len(d.toolBlocks))
		for _, block := range d.toolBlocks {
			indices = append(indices, block)
		}
		sort.Ints(indices)
		for _, block := range indices {
			d.queue = append(d.queue, ir.StreamEvent{Type: ir.BlockStop, Index: block})
		}
		d.toolBlocks = nil
	}
	// A chunk may carry usage alongside or without choices (stream_options
	// include_usage, O4); it closes out the MessageDelta.
	d.emitUsage(w.Usage)
}

func (d *streamDecoder) emitUsage(u *wireUsage) {
	if u == nil {
		return
	}
	d.queue = append(d.queue, d.terminalMessageDelta(usageFromWire(*u)))
	d.pendingMD = false
}
