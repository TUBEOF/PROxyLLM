package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/proxyllm/proxyllm/internal/translate"
	"github.com/proxyllm/proxyllm/internal/translate/ir"
)

// streamEncoder encodes canonical IR events into the Ollama /api/chat NDJSON
// stream wire format (pinned via the golden fixture ollama/chat-stream.ndjson,
// OP-4; facts L10-L11): one standalone JSON object per line, no SSE framing.
// Wire conformance is enforced via round trips through the package's
// fixture-verified stream decoder.
//
// Unlike the decoder (which infers block transitions from payload kinds),
// the encoder INVERTS the implicit framing: each line carries at most one
// payload kind, and the canonical block structure tells the encoder when a
// new line kind begins. Mapping (the inverse of streamDecoder.mapChunk):
//
//   - Start → the framing line: message with empty content and done:false.
//     The wire carries no block-open events; this line announces the model
//     and starts the stream (the decoder emits Start from it).
//   - BlockStart(text/thinking) → nothing (framing is payload-driven)
//   - BlockStart(tool_use) → nothing here; the accumulated tool call rides
//     the line of its FIRST argument fragment (below)
//   - Delta(text) → a content line (message.content = the fragment)
//   - Delta(thinking) → a thinking line (message.thinking = the fragment)
//   - Delta(tool_input) → tool fragments accumulate; the tool line carries
//     message.tool_calls[{function:{name, arguments}}] and is emitted once
//     the accumulated arguments form a complete JSON value (the wire delivers
//     exactly one complete JSON object per entry, L10 — IR fragments are
//     assembled, partial JSON never reaches the wire split, because
//     json.Marshal validates RawMessage content)
//   - BlockStop(tool) → the tool line if the accumulated arguments never
//     became complete JSON (defensive: the entry must not be lost); other
//     kinds close implicitly on the next line's payload
//   - MessageDelta → the done line: done:true, done_reason via
//     stopReasonToWire, and the usage counts (prompt_eval_count,
//     prompt_eval_cached_count, eval_count, L11). Durations are provider
//     telemetry with no IR channel and are never emitted.
//   - Done → the done line if MessageDelta did not arrive (the wire requires
//     a terminal done:true line; without it a downstream decoder hangs until
//     EOF), with done_reason defaulted per stopReasonToWire
//   - Error/Ping/Comment/unknown → nothing (no NDJSON wire channel; the
//     degradation is relay-layer concern, never a stream failure)
//
// Tool-call losses on this wire (by design, mirroring the non-stream wire):
// no tool IDs (the wire format has no id field) and no fragment boundaries
// (arguments are delivered as one complete JSON object per entry).
//
// Each Write flushes when the sink implements http.Flusher (C2: relay
// as-it-arrives).
type streamEncoder struct {
	w io.Writer

	model string

	// toolName/toolArgs assemble the open tool block's wire entry from IR
	// fragments; toolOpen reports whether an entry is pending, toolSent
	// whether its line was already emitted (BlockStop must not double-emit).
	toolName string
	toolArgs strings.Builder
	toolOpen bool
	toolSent bool

	// done reports whether the done line was emitted (MessageDelta or Done).
	done bool
}

// NewStreamEncoder returns the Ollama NDJSON stream encoder for w.
func NewStreamEncoder(w io.Writer) translate.StreamEncoder {
	return &streamEncoder{w: w}
}

// Write encodes one canonical event onto the wire.
func (e *streamEncoder) Write(ctx context.Context, ev ir.StreamEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch ev.Type {
	case ir.Start:
		if ev.Response != nil {
			e.model = ev.Response.Model
		}
		// Framing line: empty content announces model and stream start.
		return e.writeLine(wireResponse{
			Model:   e.model,
			Message: wireMessage{Role: "assistant"},
		})
	case ir.BlockStart:
		if tc, ok := ev.Part.(ir.ToolCallPart); ok {
			// Tool blocks ride the first argument fragment's line; remember
			// the entry until the fragment arrives.
			e.toolOpen = true
			e.toolSent = false
			e.toolName = tc.Name
			e.toolArgs.Reset()
		}
		return nil
	case ir.Delta:
		switch ev.Delta.Kind {
		case ir.DeltaText:
			return e.writeLine(wireResponse{
				Model:   e.model,
				Message: wireMessage{Role: "assistant", Content: ev.Delta.Text},
			})
		case ir.DeltaThinking:
			return e.writeLine(wireResponse{
				Model:   e.model,
				Message: wireMessage{Role: "assistant", Thinking: ev.Delta.Text},
			})
		case ir.DeltaToolInput:
			if !e.toolOpen {
				return nil // fragment for a tool block we never saw open
			}
			e.toolArgs.WriteString(ev.Delta.PartialJSON)
			// The wire carries exactly one complete JSON object per
			// tool_calls entry (L10); emit only once the assembled
			// arguments are a complete value. json.Marshal validates
			// RawMessage content, so partial JSON must never ride the wire.
			if !json.Valid([]byte(e.toolArgs.String())) {
				return nil
			}
			e.toolSent = true
			return e.emitToolLine()
		default:
			// signature deltas have no wire channel (signatures are an
			// Anthropic-only concept; never fabricated, A12).
			return nil
		}
	case ir.BlockStop:
		// Implicit framing: other kinds close implicitly on the next line's
		// payload. Closing the tool block releases the assembled entry —
		// defensively emitting it if the accumulated arguments never became
		// complete JSON (a truncated upstream entry must not be lost
		// silently; the decoder will surface the invalid arguments).
		var err error
		if e.toolOpen {
			e.toolOpen = false
			if e.toolArgs.Len() > 0 && !e.toolSent {
				err = e.emitToolLine()
			}
			e.toolSent = false
			e.toolName = ""
			e.toolArgs.Reset()
		}
		return err
	case ir.MessageDelta:
		return e.writeDone(ev.MessageDelta)
	case ir.Done:
		if e.done {
			return nil
		}
		return e.writeDone(nil)
	case ir.Error, ir.Ping, ir.Comment:
		// No NDJSON wire channel for errors or keepalives (the wire format
		// has no error event, L10); the stream stays valid. Downstream
		// consumers see the stream end at the next terminal event.
		return nil
	default:
		// Unknown event kinds stay off the wire (A5 discipline).
		return nil
	}
}

// emitToolLine writes the assembled tool_calls entry as one line. Callers
// only invoke it with complete JSON arguments (or defensively at BlockStop
// to avoid losing a truncated entry).
func (e *streamEncoder) emitToolLine() error {
	return e.writeLine(wireResponse{
		Model: e.model,
		Message: wireMessage{
			Role: "assistant",
			ToolCalls: []wireToolCall{{
				Function: wireToolCallFunction{
					Name:      e.toolName,
					Arguments: json.RawMessage(e.toolArgs.String()),
				},
			}},
		},
	})
}

// writeDone emits the terminal done line from the message-delta payload (or
// defaults when Done arrives without one).
func (e *streamEncoder) writeDone(md *ir.MessageDeltaPayload) error {
	if e.done {
		return nil
	}
	e.done = true
	reason := ir.StopEndTurn
	var u ir.Usage
	if md != nil {
		if md.StopReason != "" {
			reason = md.StopReason
		}
		u = md.Usage
	}
	return e.writeLine(wireResponse{
		Model:                 e.model,
		Done:                  true,
		DoneReason:            stopReasonToWire(reason),
		Message:               wireMessage{Role: "assistant"},
		PromptEvalCount:       u.InputTokens,
		PromptEvalCachedCount: u.CacheReadInputTokens,
		EvalCount:             u.OutputTokens,
	})
}

// writeLine marshals one NDJSON object, writes it with its newline, and
// flushes (C2). It stamps created_at with the runtime timestamp (the wire
// timestamp of this response).
func (e *streamEncoder) writeLine(w wireResponse) error {
	w.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	raw, err := json.Marshal(w)
	if err != nil {
		return fmt.Errorf("ollama: encode stream line: %w", err)
	}
	if _, err := io.WriteString(e.w, string(raw)+"\n"); err != nil {
		return fmt.Errorf("ollama: encode stream: %w", err)
	}
	if f, ok := e.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}
