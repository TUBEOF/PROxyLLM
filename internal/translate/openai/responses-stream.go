package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/tubeof/proxyllm/internal/translate"
	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/sse"
)

// responsesStreamDecoder decodes the Responses-API SSE stream
// (developers.openai.com/api/docs/api-reference/responses-streaming, accessed
// 2026-09-10) into canonical IR events, using the shared WHATWG SSE reader.
//
// Unlike chat-completions SSE (data-only), the Responses stream uses named
// events (`event:` lines). The machine (canonical mapping, documented in the
// responses-stream fixture sidecar):
//   - response.created → Start (full response snapshot in .response; usage
//     null until response.completed)
//   - response.in_progress → skip (redundant snapshot)
//   - response.output_item.added → BlockStart (text block for a message
//     item; tool_use block from a function_call item's {call_id, name});
//     reasoning items open a thinking block
//   - response.content_part.added → skip (redundant with output_item.added)
//   - response.output_text.delta → Delta(text)
//   - response.reasoning_summary_text.delta → Delta(thinking)
//   - response.function_call_arguments.delta → Delta(tool_input)
//   - response.output_text.done / response.content_part.done /
//     response.function_call_arguments.done → skip (content already
//     delivered via deltas)
//   - response.output_item.done → BlockStop
//   - response.completed → MessageDelta(stop reason from the final output,
//     usage from the snapshot) + Done
//   - error (FLAT {code, message, param}) → terminal ir.Error
//   - response.failed → terminal ir.Error from the snapshot's error object
//   - unknown event types → Comment (pass through unlogged
//     semantics, never fail)
//
// One wire event can map to two canonical events (completed → MessageDelta +
// Done), so decoded events queue and Read hands them out one at a time.
type responsesStreamDecoder struct {
	reader *sse.Reader

	// queue holds canonical events decoded but not yet returned.
	queue []ir.StreamEvent

	started  bool // Start emitted
	openItem int  // output_index of the open item; -1 = none
	blockCnt int  // blocks opened so far (next IR index)
	hasTool  bool // any function_call item seen (stop-reason inference)
	finish   ir.StopReason
	done     bool // terminal state reached
}

func newResponsesStreamDecoder(r io.Reader) translate.StreamDecoder {
	return &responsesStreamDecoder{
		reader:   sse.NewReader(r),
		openItem: -1,
	}
}

// Read returns the next canonical event. It returns io.EOF after the
// terminal event (completed/failed/error) has been handed out, and at clean
// stream end.
func (d *responsesStreamDecoder) Read(_ context.Context) (ir.StreamEvent, error) {
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
			return ir.StreamEvent{}, fmt.Errorf("openai/responses: stream read: %w", err)
		}
		if ev.IsComment {
			return ir.StreamEvent{Type: ir.Comment}, nil
		}
		var w responsesWireStreamEvent
		if err := json.Unmarshal([]byte(ev.Data), &w); err != nil {
			return ir.StreamEvent{}, fmt.Errorf("openai/responses: decode stream event: %w", err)
		}
		d.mapEvent(&w)
		if len(d.queue) > 0 {
			out := d.queue[0]
			d.queue = d.queue[1:]
			return out, nil
		}
		// The event carried nothing canonical; read the next one.
	}
}

// responsesWireStreamEvent is the union shape of the named Responses stream
// events. The event name rides in `type`; each payload family sets only
// its own fields.
type responsesWireStreamEvent struct {
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number,omitempty"`

	// response.created / in_progress / completed / failed carry the full
	// response snapshot in .response.
	Response *responsesWireResponse `json:"response,omitempty"`

	// output_item.added / done.
	OutputIndex int                `json:"output_index,omitempty"`
	Item        *responsesWireItem `json:"item,omitempty"`

	// content_part.added / done, output_text.delta / done.
	ItemID       string             `json:"item_id,omitempty"`
	ContentIndex int                `json:"content_index,omitempty"`
	Part         *responsesWirePart `json:"part,omitempty"`
	Delta        string             `json:"delta,omitempty"`
	Text         string             `json:"text,omitempty"`

	// function_call_arguments.delta / done.
	Arguments string `json:"arguments,omitempty"`

	// reasoning_summary_text.delta / done.
	SummaryIndex int `json:"summary_index,omitempty"`

	// error (FLAT — no nested error object).
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Param   string `json:"param,omitempty"`
}

func (d *responsesStreamDecoder) terminalMessageDelta(u ir.Usage) ir.StreamEvent {
	return ir.StreamEvent{Type: ir.MessageDelta, MessageDelta: &ir.MessageDeltaPayload{
		StopReason: d.finish,
		Usage:      u,
	}}
}

// mapEvent appends the canonical events for one wire event to the queue.
func (d *responsesStreamDecoder) mapEvent(w *responsesWireStreamEvent) {
	switch w.Type {
	case "response.created", "response.in_progress":
		if !d.started && w.Response != nil {
			// Start from the full snapshot; usage stays zero until
			// response.completed.
			d.started = true
			d.queue = append(d.queue, ir.StreamEvent{Type: ir.Start, Response: &ir.Response{
				ID:    w.Response.ID,
				Model: w.Response.Model,
			}})
		}
	case "response.output_item.added":
		// Close any still-open block before opening the next (the wire
		// normally sends output_item.done first; this is defensive).
		if d.openItem >= 0 {
			d.queue = append(d.queue, ir.StreamEvent{Type: ir.BlockStop, Index: d.blockCnt - 1})
		}
		d.openItem = w.OutputIndex
		switch {
		case w.Item != nil && w.Item.Type == "function_call":
			d.hasTool = true
			d.blockCnt++
			d.queue = append(d.queue, ir.StreamEvent{Type: ir.BlockStart, Index: d.blockCnt - 1,
				Part: ir.ToolCallPart{ID: w.Item.CallID, Name: w.Item.Name}})
		case w.Item != nil && w.Item.Type == "reasoning":
			d.blockCnt++
			d.queue = append(d.queue, ir.StreamEvent{Type: ir.BlockStart, Index: d.blockCnt - 1,
				Part: ir.ThinkingPart{}})
		case w.Item != nil && w.Item.Type == "message":
			// Message items open a text block; content follows via
			// output_text.delta.
			d.blockCnt++
			d.queue = append(d.queue, ir.StreamEvent{Type: ir.BlockStart, Index: d.blockCnt - 1,
				Part: ir.TextPart{}})
		}
	case "response.content_part.added":
		// Redundant with output_item.added; skip.
	case "response.output_text.delta":
		d.delta(w.OutputIndex, w.Delta, ir.DeltaText)
	case "response.reasoning_summary_text.delta":
		d.delta(w.OutputIndex, w.Delta, ir.DeltaThinking)
	case "response.function_call_arguments.delta":
		d.delta(w.OutputIndex, w.Delta, ir.DeltaToolInput)
	case "response.output_text.done", "response.content_part.done",
		"response.function_call_arguments.done", "response.reasoning_summary_text.done":
		// Redundant snapshots; content already delivered via deltas.
	case "response.output_item.done":
		if d.openItem >= 0 {
			d.queue = append(d.queue, ir.StreamEvent{Type: ir.BlockStop, Index: d.blockCnt - 1})
			d.openItem = -1
		}
	case "response.completed":
		if !d.started && w.Response != nil {
			d.started = true
			d.queue = append(d.queue, ir.StreamEvent{Type: ir.Start, Response: &ir.Response{
				ID:    w.Response.ID,
				Model: w.Response.Model,
			}})
		}
		if d.openItem >= 0 {
			d.queue = append(d.queue, ir.StreamEvent{Type: ir.BlockStop, Index: d.blockCnt - 1})
			d.openItem = -1
		}
		if d.finish == "" {
			// Status completed → end_turn unless the final output holds
			// function calls.
			if w.Response != nil && w.Response.Status == "incomplete" {
				d.finish = ir.StopMaxTokens
			} else if d.hasTool {
				d.finish = ir.StopToolUse
			} else {
				d.finish = ir.StopEndTurn
			}
		}
		var usage ir.Usage
		if w.Response != nil {
			usage = responsesUsageFromWire(w.Response.Usage)
		}
		d.queue = append(d.queue, d.terminalMessageDelta(usage))
		d.queue = append(d.queue, ir.StreamEvent{Type: ir.Done})
		d.done = true
	case "error":
		// FLAT error event: {code, message, param} at top level.
		d.queue = append(d.queue, ir.StreamEvent{Type: ir.Error, Error: &ir.UpstreamError{
			Code:    w.Code,
			Message: w.Message,
		}})
		d.done = true
	case "response.failed":
		if !d.started && w.Response != nil {
			d.started = true
			d.queue = append(d.queue, ir.StreamEvent{Type: ir.Start, Response: &ir.Response{
				ID:    w.Response.ID,
				Model: w.Response.Model,
			}})
		}
		if w.Response != nil && w.Response.Error != nil {
			d.queue = append(d.queue, ir.StreamEvent{Type: ir.Error, Error: &ir.UpstreamError{
				Code:    w.Response.Error.Code,
				Message: w.Response.Error.Message,
			}})
		} else {
			d.queue = append(d.queue, ir.StreamEvent{Type: ir.Error, Error: &ir.UpstreamError{
				Message: "response failed",
			}})
		}
		d.done = true
	default:
		// Unknown event types pass through as comments.
		d.queue = append(d.queue, ir.StreamEvent{Type: ir.Comment})
	}
}

// delta appends a Delta event for the open item at outputIndex.
func (d *responsesStreamDecoder) delta(outputIndex int, text string, kind ir.DeltaKind) {
	if d.openItem < 0 || text == "" || outputIndex != d.openItem {
		return
	}
	dp := ir.DeltaPayload{Kind: kind}
	switch kind {
	case ir.DeltaToolInput:
		dp.PartialJSON = text
	default:
		dp.Text = text
	}
	d.queue = append(d.queue, ir.StreamEvent{Type: ir.Delta, Index: d.blockCnt - 1, Delta: dp})
}
