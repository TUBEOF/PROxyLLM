package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/tubeof/proxyllm/internal/translate"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// streamEncoder encodes canonical IR events into the Anthropic Messages SSE
// wire format (docs.anthropic.com/en/api/messages-streaming, accessed
// 2026-09-10): named `event:` lines with one JSON `data:` payload,
// each event terminated by a dispatching blank line. Wire conformance is
// pinned by the golden fixture anthropic/stream-events.sse and enforced via
// round trips through the package's fixture-verified stream decoder.
//
// Mapping (the exact inverse of streamDecoder.mapEvent):
//   - Start → message_start (full message snapshot; stop_reason/stop_sequence
//     emitted as explicit nulls, matching the fixture's wire shape)
//   - Ping → ping
//   - BlockStart → content_block_start (text/thinking with empty-string
//     init content, tool_use with an empty `input` object)
//   - Delta → content_block_delta (text_delta, thinking_delta,
//     signature_delta, input_json_delta)
//   - BlockStop → content_block_stop
//   - MessageDelta → message_delta (stop_reason + stop_sequence:null in
//     .delta, usage at the top level)
//   - Done → message_stop
//   - Error → error (nested {type, message} object)
//   - Comment → a wire comment line (`:…`, kept alive by relay consumers)
//   - unknown event/delta kinds → a wire comment line (never fail the stream
//     over a kind this build does not know)
//
// Each Write flushes when the sink implements http.Flusher, so bytes reach
// the client as they arrive.
type streamEncoder struct {
	w io.Writer
}

// NewStreamEncoder returns the Anthropic Messages SSE stream encoder for w.
func NewStreamEncoder(w io.Writer) translate.StreamEncoder {
	return &streamEncoder{w: w}
}

// Write encodes one canonical event onto the wire.
func (e *streamEncoder) Write(ctx context.Context, ev ir.StreamEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name, data, err := e.encode(ev)
	if err != nil {
		return err
	}
	if name == "" {
		// Comments and unknown kinds relay as wire comment lines.
		_, err = io.WriteString(e.w, ":\n\n")
	} else {
		_, err = fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", name, data)
	}
	if err != nil {
		return fmt.Errorf("anthropic: encode stream event %s: %w", ev.Type, err)
	}
	if f, ok := e.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// encode maps one canonical event onto (event name, JSON payload). An empty
// name means "wire comment".
func (e *streamEncoder) encode(ev ir.StreamEvent) (string, json.RawMessage, error) {
	switch ev.Type {
	case ir.Start:
		resp := ir.Response{}
		if ev.Response != nil {
			resp = *ev.Response
		}
		return marshal(wireStartEvent{Type: "message_start", Message: wireStartMessage{
			ID:      resp.ID,
			Type:    "message",
			Role:    "assistant",
			Model:   resp.Model,
			Content: []wireBlock{},
			Usage:   usageToWire(resp.Usage),
		}})
	case ir.Ping:
		return marshal(wirePingEvent{Type: "ping"})
	case ir.BlockStart:
		blk, err := encodeContentBlock(ev.Part)
		if err != nil {
			return "", nil, err
		}
		return marshal(wireBlockStartEvent{Type: "content_block_start", Index: ev.Index, ContentBlock: blk})
	case ir.Delta:
		d, ok := encodeStreamDelta(ev.Delta)
		if !ok {
			// Unknown delta kinds tolerate like unknown wire types.
			return "", nil, nil
		}
		return marshal(wireBlockDeltaEvent{Type: "content_block_delta", Index: ev.Index, Delta: d})
	case ir.BlockStop:
		return marshal(wireBlockStopEvent{Type: "content_block_stop", Index: ev.Index})
	case ir.MessageDelta:
		md := ev.MessageDelta
		if md == nil {
			return "", nil, nil
		}
		var seq *string
		if md.StopSequence != "" {
			s := md.StopSequence
			seq = &s
		}
		return marshal(wireMessageDeltaEvent{
			Type: "message_delta",
			Delta: wireMessageDeltaDelta{
				StopReason:   md.StopReason,
				StopSequence: seq,
			},
			Usage: usageToWire(md.Usage),
		})
	case ir.Done:
		return marshal(wireMessageStopEvent{Type: "message_stop"})
	case ir.Error:
		we := wireStreamError{}
		if ev.Error != nil {
			we = wireStreamError{Type: ev.Error.Type, Message: ev.Error.Message}
		}
		return marshal(wireErrorEvent{Type: "error", Error: we})
	default:
		// Unknown event kinds relay as wire comments.
		return "", nil, nil
	}
}

// marshal serializes an event payload and pairs it with its wire name.
func marshal(v any) (string, json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", nil, fmt.Errorf("anthropic: marshal stream event: %w", err)
	}
	return wireEventName(v), raw, nil
}

// encodeContentBlock renders the content_block payload for a BlockStart
// event: empty-init blocks per the fixture (text "", thinking "", tool_use
// with `"input":{}`), any other part via the shared block conversion.
func encodeContentBlock(p ir.Part) (json.RawMessage, error) {
	switch pt := p.(type) {
	case ir.TextPart:
		return json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{Type: "text", Text: pt.Text})
	case ir.ThinkingPart:
		return json.Marshal(struct {
			Type     string `json:"type"`
			Thinking string `json:"thinking"`
		}{Type: "thinking", Thinking: pt.Thinking})
	case ir.ToolCallPart:
		input := pt.Input
		if len(input) == 0 || string(input) == "null" {
			input = json.RawMessage("{}")
		}
		return json.Marshal(struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}{Type: "tool_use", ID: pt.ID, Name: pt.Name, Input: input})
	case ir.ServerToolUsePart:
		// Same empty-init discipline as tool_use: the wire
		// content_block_start carries "input":{} and the input arrives
		// via input_json_delta, as documented under
		// platform.claude.com/docs/en/build-with-claude/streaming — never
		// omitted, despite the omitempty on the shared wireBlock.
		input := pt.Input
		if len(input) == 0 || string(input) == "null" {
			input = json.RawMessage("{}")
		}
		return json.Marshal(struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}{Type: "server_tool_use", ID: pt.ID, Name: pt.Name, Input: input})
	default:
		raw, err := json.Marshal(blockFromPart(p))
		if err != nil {
			return nil, fmt.Errorf("anthropic: marshal content block: %w", err)
		}
		return raw, nil
	}
}

// encodeStreamDelta maps a delta payload onto its wire delta type.
func encodeStreamDelta(d ir.DeltaPayload) (wireStreamDelta, bool) {
	switch d.Kind {
	case ir.DeltaText:
		return wireStreamDelta{Type: "text_delta", Text: d.Text}, true
	case ir.DeltaThinking:
		return wireStreamDelta{Type: "thinking_delta", Thinking: d.Text}, true
	case ir.DeltaSignature:
		return wireStreamDelta{Type: "signature_delta", Signature: d.Text}, true
	case ir.DeltaToolInput:
		return wireStreamDelta{Type: "input_json_delta", PartialJSON: d.PartialJSON}, true
	default:
		return wireStreamDelta{}, false
	}
}

// wireEventName reports the SSE event name for a payload type.
func wireEventName(v any) string {
	switch v.(type) {
	case wireStartEvent:
		return "message_start"
	case wirePingEvent:
		return "ping"
	case wireBlockStartEvent:
		return "content_block_start"
	case wireBlockDeltaEvent:
		return "content_block_delta"
	case wireBlockStopEvent:
		return "content_block_stop"
	case wireMessageDeltaEvent:
		return "message_delta"
	case wireMessageStopEvent:
		return "message_stop"
	case wireErrorEvent:
		return "error"
	default:
		return ""
	}
}

// ---- encoder wire types ----
//
// Unlike the decoder's wireStreamEvent (a union whose `index` is always
// present), each encoder event gets its own struct so every field the
// fixture shows is emitted — and nothing else (message_stop must not carry
// a stray "index":0).

// wireStartMessage is the message snapshot inside message_start; unlike
// wireResponse it emits stop_reason/stop_sequence as explicit nulls, as the
// fixture's wire shape requires.
type wireStartMessage struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Model        string         `json:"model"`
	Content      []wireBlock    `json:"content"`
	StopReason   *ir.StopReason `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        wireUsage      `json:"usage"`
}

type wireStartEvent struct {
	Type    string           `json:"type"`
	Message wireStartMessage `json:"message"`
}

type wirePingEvent struct {
	Type string `json:"type"`
}

type wireBlockStartEvent struct {
	Type         string          `json:"type"`
	Index        int             `json:"index"`
	ContentBlock json.RawMessage `json:"content_block"`
}

type wireBlockDeltaEvent struct {
	Type  string          `json:"type"`
	Index int             `json:"index"`
	Delta wireStreamDelta `json:"delta"`
}

type wireBlockStopEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

// wireMessageDeltaDelta is the .delta object of message_delta;
// stop_sequence is always present (null when unused) per the fixture.
type wireMessageDeltaDelta struct {
	StopReason   ir.StopReason `json:"stop_reason"`
	StopSequence *string       `json:"stop_sequence"`
}

type wireMessageDeltaEvent struct {
	Type  string                `json:"type"`
	Delta wireMessageDeltaDelta `json:"delta"`
	Usage wireUsage             `json:"usage"`
}

type wireMessageStopEvent struct {
	Type string `json:"type"`
}

type wireErrorEvent struct {
	Type  string          `json:"type"`
	Error wireStreamError `json:"error"`
}
