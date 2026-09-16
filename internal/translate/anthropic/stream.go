package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/tubeof/proxyllm/internal/translate"
	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/sse"
)

// streamDecoder decodes the Anthropic Messages SSE stream (facts A5–A7,
// A13) into canonical IR events, using the shared WHATWG SSE reader.
type streamDecoder struct {
	reader *sse.Reader
	// done reports whether message_stop (Done) or a terminal error event
	// has been emitted; the decoder yields io.EOF afterwards.
	done bool
}

func newStreamDecoder(r io.Reader) translate.StreamDecoder {
	return &streamDecoder{reader: sse.NewReader(r)}
}

// Read returns the next canonical event. It returns io.EOF after Done or a
// terminal Error event, and at clean stream end.
func (d *streamDecoder) Read(_ context.Context) (ir.StreamEvent, error) {
	if d.done {
		return ir.StreamEvent{}, io.EOF
	}
	// Anthropic stream events map 1:1 onto canonical events — no event
	// queue needed, unlike the chat-completions chunk machines.
	ev, err := d.reader.Next()
	if err == io.EOF {
		return ir.StreamEvent{}, io.EOF
	}
	if err != nil {
		return ir.StreamEvent{}, fmt.Errorf("anthropic: stream read: %w", err)
	}
	if ev.IsComment {
		// Comments relay verbatim; the Comment event type carries no
		// payload at IR level (wire bytes stay at the relay layer).
		return ir.StreamEvent{Type: ir.Comment}, nil
	}
	out, err := d.mapEvent(ev.Data)
	if err != nil {
		return ir.StreamEvent{}, err
	}
	if out.Type == ir.Done || out.Type == ir.Error {
		d.done = true
	}
	return out, nil
}

// wireStreamEvent mirrors the Anthropic stream event JSON envelope.
type wireStreamEvent struct {
	Type         string           `json:"type"`
	Message      *wireResponse    `json:"message,omitempty"`
	Index        int              `json:"index"`
	ContentBlock *wireBlock       `json:"content_block,omitempty"`
	Delta        *wireStreamDelta `json:"delta,omitempty"`
	Usage        *wireUsage       `json:"usage,omitempty"`
	Error        *wireStreamError `json:"error,omitempty"`
}

type wireStreamDelta struct {
	Type        string        `json:"type"`
	Text        string        `json:"text,omitempty"`
	Thinking    string        `json:"thinking,omitempty"`
	Signature   string        `json:"signature,omitempty"`
	PartialJSON string        `json:"partial_json,omitempty"`
	StopReason  ir.StopReason `json:"stop_reason,omitempty"`
}

type wireStreamError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// mapEvent converts one dispatched SSE event's JSON data into a canonical
// event. Unknown event types surface as Comment events (A5: clients must
// tolerate future types; the relay logs them).
func (d *streamDecoder) mapEvent(data string) (ir.StreamEvent, error) {
	var w wireStreamEvent
	if err := json.Unmarshal([]byte(data), &w); err != nil {
		return ir.StreamEvent{}, fmt.Errorf("anthropic: decode stream event: %w", err)
	}
	switch w.Type {
	case "message_start":
		resp := ir.Response{}
		if w.Message != nil {
			resp.ID = w.Message.ID
			resp.Model = w.Message.Model
			resp.Usage = w.Message.Usage.toIR()
		}
		return ir.StreamEvent{Type: ir.Start, Response: &resp}, nil
	case "ping":
		return ir.StreamEvent{Type: ir.Ping}, nil
	case "content_block_start":
		ev := ir.StreamEvent{Type: ir.BlockStart, Index: w.Index}
		if w.ContentBlock != nil {
			if p, ok := partFromBlock(*w.ContentBlock); ok {
				ev.Part = p
			}
		}
		return ev, nil
	case "content_block_delta":
		ev := ir.StreamEvent{Type: ir.Delta, Index: w.Index}
		if w.Delta != nil {
			switch w.Delta.Type {
			case "text_delta":
				ev.Delta = ir.DeltaPayload{Kind: ir.DeltaText, Text: w.Delta.Text}
			case "thinking_delta":
				ev.Delta = ir.DeltaPayload{Kind: ir.DeltaThinking, Text: w.Delta.Thinking}
			case "signature_delta":
				ev.Delta = ir.DeltaPayload{Kind: ir.DeltaSignature, Text: w.Delta.Signature}
			case "input_json_delta":
				ev.Delta = ir.DeltaPayload{Kind: ir.DeltaToolInput, PartialJSON: w.Delta.PartialJSON}
			default:
				// Unknown delta types tolerate the same way unknown event
				// types do (A5 discipline applies to nested discriminators).
				return ir.StreamEvent{Type: ir.Comment}, nil
			}
		}
		return ev, nil
	case "content_block_stop":
		return ir.StreamEvent{Type: ir.BlockStop, Index: w.Index}, nil
	case "message_delta":
		md := &ir.MessageDeltaPayload{}
		if w.Delta != nil {
			md.StopReason = w.Delta.StopReason
		}
		if w.Usage != nil {
			md.Usage = w.Usage.toIR()
		}
		return ir.StreamEvent{Type: ir.MessageDelta, MessageDelta: md}, nil
	case "message_stop":
		return ir.StreamEvent{Type: ir.Done}, nil
	case "error":
		ue := &ir.UpstreamError{}
		if w.Error != nil {
			ue.Type = w.Error.Type
			ue.Message = w.Error.Message
		}
		return ir.StreamEvent{Type: ir.Error, Error: ue}, nil
	default:
		return ir.StreamEvent{Type: ir.Comment}, nil
	}
}
