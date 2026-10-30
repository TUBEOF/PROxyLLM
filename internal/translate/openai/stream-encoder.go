package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/tubeof/proxyllm/internal/translate"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// streamEncoder encodes canonical IR events into the OpenAI
// chat-completions SSE wire format
// (developers.openai.com/api/docs/guides/streaming, accessed 2026-09-10):
// data-only SSE — every event is one `data: {chunk}` line and a dispatching
// blank line — terminated by the `data: [DONE]` sentinel. Wire conformance
// is pinned by the golden fixture openai/chat-stream.sse and enforced via
// round trips through the fixture-verified stream decoder.
//
// Mapping (the exact inverse of streamDecoder.mapChunk):
//   - Start → the role chunk (`delta:{role:"assistant",content:""}`); on
//     this wire it also implies the initial text block, which the decoder
//     re-opens on the first chunk
//   - BlockStart(text) → nothing (implied by the role chunk)
//   - BlockStart(tool_use) → a tool_calls delta carrying the wire index,
//     id, name and an empty arguments string (the assembly anchor); on
//     this wire it also implies the BlockStop of the previously open
//     block, which the decoder re-emits on the index change
//   - Delta(text) → a content delta chunk
//   - Delta(tool_input) → a tool_calls argument-fragment chunk (wire
//     index + function.arguments only)
//   - Delta(thinking/signature) → nothing (no chat wire channel; the
//     per-model compat policy decides the degradation — never fabricated)
//   - BlockStop → deferred: the finish_reason chunk closes the last open
//     block, and the stop reason is only known at MessageDelta
//   - MessageDelta → the deferred finish_reason chunk (if a BlockStop is
//     pending), then the usage chunk (a choice with an empty delta plus
//     the top-level usage object, stream_options.include_usage)
//   - Done → the deferred finish_reason chunk (defensively), then
//     `data: [DONE]`
//   - Error → a chunk carrying a top-level error object
//   - Ping/Comment/unknown kinds → a wire comment line (the chat wire has
//     no ping channel; never fail the stream)
//
// Each Write flushes when the sink implements http.Flusher, so bytes reach
// the client as they arrive.
type streamEncoder struct {
	w io.Writer

	id      string
	model   string
	created int64

	toolIndex int // wire tool_calls index of the open tool block
	hasTool   bool
	// toolOpen is true while a ToolCallPart block is the current block. The
	// emulation loop also emits DeltaToolInput for ServerToolUsePart blocks
	// (a family the chat wire cannot represent); without this gate those
	// fragments reached the client as tool_calls deltas with no id/name and
	// strict parsers (OpenCode, Cline) rejected the whole stream.
	toolOpen bool

	// pendingStop: a BlockStop was seen whose finish_reason chunk is not
	// yet emitted (the stop reason arrives with MessageDelta).
	pendingStop bool
}

// NewStreamEncoder returns the OpenAI chat-completions SSE stream encoder
// for w.
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
			e.id = ev.Response.ID
			e.model = ev.Response.Model
		}
		return e.emit(e.chunk([]encChoice{{
			Index: 0,
			Delta: &encDelta{Role: "assistant", Content: ptrString("")},
		}}, nil, nil))
	case ir.BlockStart:
		e.toolOpen = false
		if tc, ok := ev.Part.(ir.ToolCallPart); ok {
			e.toolOpen = true
			if e.hasTool {
				// A second tool block shifts the wire index; the first is 0.
				e.toolIndex++
			}
			e.hasTool = true
			// The anchor may already carry complete arguments (an IR
			// assembly that decoded Input into the block start — e.g. a
			// one-shot tool call): emit them here, never a lone "" that
			// loses the payload (inverse of the decode-side one-shot loss).
			args := ""
			if len(tc.Input) > 0 && string(tc.Input) != "null" {
				args = string(tc.Input)
			}
			return e.emit(e.chunk([]encChoice{{
				Index: 0,
				Delta: &encDelta{ToolCalls: []encToolCall{{
					Index:    e.toolIndex,
					ID:       tc.ID,
					Type:     "function",
					Function: encFunction{Name: tc.Name, Arguments: ptrString(args)},
				}}},
			}}, nil, nil))
		}
		// Text/thinking blocks are implied by the role chunk.
		return nil
	case ir.Delta:
		switch ev.Delta.Kind {
		case ir.DeltaText:
			return e.emit(e.chunk([]encChoice{{
				Index: 0,
				Delta: &encDelta{Content: ptrString(ev.Delta.Text)},
			}}, nil, nil))
		case ir.DeltaToolInput:
			if !e.toolOpen {
				// A ServerToolUsePart's input has no chat-wire channel and no
				// anchor was emitted for it; a fragment here would corrupt
				// every client that assembles tool calls.
				return nil
			}
			return e.emit(e.chunk([]encChoice{{
				Index: 0,
				Delta: &encDelta{ToolCalls: []encToolCall{{
					Index:    e.toolIndex,
					Function: encFunction{Arguments: ptrString(ev.Delta.PartialJSON)},
				}}},
			}}, nil, nil))
		default:
			// thinking/signature deltas have no chat wire channel.
			return nil
		}
	case ir.BlockStop:
		// Deferred: the finish_reason chunk is emitted at MessageDelta (or
		// Done, defensively) when the stop reason is known.
		e.toolOpen = false
		e.pendingStop = true
		return nil
	case ir.MessageDelta:
		var md *ir.MessageDeltaPayload
		var u ir.Usage
		if ev.MessageDelta != nil {
			md = ev.MessageDelta
			u = md.Usage
		}
		if e.pendingStop {
			e.pendingStop = false
			if err := e.emit(e.chunk([]encChoice{{
				Index:        0,
				Delta:        &encDelta{},
				FinishReason: ptrString(finishOf(md, e.hasTool)),
			}}, nil, nil)); err != nil {
				return err
			}
		}
		return e.emit(e.chunk(
			[]encChoice{{Index: 0, Delta: &encDelta{}}},
			encUsageFromIR(u), nil))
	case ir.Done:
		if e.pendingStop {
			e.pendingStop = false
			if err := e.emit(e.chunk([]encChoice{{
				Index:        0,
				Delta:        &encDelta{},
				FinishReason: ptrString(finishOf(nil, e.hasTool)),
			}}, nil, nil)); err != nil {
				return err
			}
		}
		return e.writeRaw("data: [DONE]\n\n")
	case ir.Error:
		we := wireAPIError{}
		if ev.Error != nil {
			we = wireAPIError{Message: ev.Error.Message, Type: ev.Error.Type, Code: ev.Error.Code}
		}
		return e.emit(e.chunk([]encChoice{}, nil, &we))
	case ir.Ping, ir.Comment:
		// The chat wire has no ping channel; relay keepalives as wire
		// comment lines (the client's watchdog counts relayed bytes).
		return e.writeRaw(":\n\n")
	default:
		// Unknown event kinds relay as wire comments.
		return e.writeRaw(":\n\n")
	}
}

// chunk assembles one wire chunk envelope. created is captured once on the
// first chunk (the wire timestamp of this response).
func (e *streamEncoder) chunk(choices []encChoice, usage *encUsage, werr *wireAPIError) encChunk {
	if e.created == 0 {
		e.created = time.Now().Unix()
	}
	return encChunk{
		ID:      e.id,
		Object:  "chat.completion.chunk",
		Created: e.created,
		Model:   e.model,
		Choices: choices,
		Usage:   usage,
		Error:   werr,
	}
}

// emit marshals one chunk, writes its data line, and flushes.
func (e *streamEncoder) emit(c encChunk) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("openai: encode stream chunk: %w", err)
	}
	return e.writeRaw("data: " + string(raw) + "\n\n")
}

// writeRaw writes one wire frame and flushes.
func (e *streamEncoder) writeRaw(s string) error {
	if _, err := io.WriteString(e.w, s); err != nil {
		return fmt.Errorf("openai: encode stream: %w", err)
	}
	if f, ok := e.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// finishOf resolves the wire finish_reason for the terminal MessageDelta:
// the canonical stop reason when set, otherwise inferred from whether a
// tool block was opened.
func finishOf(md *ir.MessageDeltaPayload, hasTool bool) string {
	reason := ir.StopEndTurn
	if md != nil && md.StopReason != "" {
		reason = md.StopReason
	} else if hasTool {
		reason = ir.StopToolUse
	}
	return stopReasonToWire(reason)
}

// ptrString boxes s so omitempty pointer fields emit present-but-empty
// values (the wire needs `"content":""` and `"arguments":""`).
func ptrString(s string) *string { return &s }

// encUsageFromIR renders usage with the fixture's shape: details objects
// always present, zeros explicit (golden chat-stream.sse line 15 and
// chat-response.json lines 29-30).
func encUsageFromIR(u ir.Usage) *encUsage {
	return &encUsage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
		PromptTokensDetails: encPromptTokensDetails{
			CachedTokens: u.CacheReadInputTokens,
		},
		CompletionTokensDetails: encCompletionTokensDetails{
			ReasoningTokens: u.ReasoningTokens,
		},
	}
}

// ---- encoder wire types ----
//
// The decoder's wireChoice/wireMessage cannot be reused here: their field
// tags exist for decoding tolerance (finish_reason as plain string,
// always-present role), while the encoder must reproduce the fixture's
// exact shapes — JSON-null finish_reason, minimal delta objects that omit
// unset members, created/model on every chunk.

// encChunk is one chat.completion.chunk envelope (field order matches the
// golden fixture: id, object, created, model, choices, usage, error).
type encChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []encChoice   `json:"choices"`
	Usage   *encUsage     `json:"usage,omitempty"`
	Error   *wireAPIError `json:"error,omitempty"`
}

// encChoice is one choice; FinishReason is a pointer so in-flight chunks
// carry JSON null and the terminal chunk carries the reason.
type encChoice struct {
	Index        int       `json:"index"`
	Delta        *encDelta `json:"delta,omitempty"`
	FinishReason *string   `json:"finish_reason"`
}

// encDelta is the minimal chunk delta: only set members are emitted
// (role+content on the role chunk, content on text chunks, tool_calls on
// tool chunks, empty object on finish/usage chunks).
type encDelta struct {
	Role      string        `json:"role,omitempty"`
	Content   *string       `json:"content,omitempty"`
	ToolCalls []encToolCall `json:"tool_calls,omitempty"`
}

// encToolCall assembles by index: the anchor entry carries id/type/
// name/empty arguments; argument fragments carry index + arguments only.
type encToolCall struct {
	Index    int         `json:"index"`
	ID       string      `json:"id,omitempty"`
	Type     string      `json:"type,omitempty"`
	Function encFunction `json:"function"`
}

// encFunction pairs the anchor name with present-but-empty arguments via
// the pointer field.
type encFunction struct {
	Name      string  `json:"name,omitempty"`
	Arguments *string `json:"arguments,omitempty"`
}

// encUsage mirrors the fixture's usage shape: totals always, detail
// objects always present with explicit zeros.
type encUsage struct {
	PromptTokens            int                        `json:"prompt_tokens"`
	CompletionTokens        int                        `json:"completion_tokens"`
	TotalTokens             int                        `json:"total_tokens"`
	PromptTokensDetails     encPromptTokensDetails     `json:"prompt_tokens_details"`
	CompletionTokensDetails encCompletionTokensDetails `json:"completion_tokens_details"`
}

// encPromptTokensDetails carries cached_tokens.
type encPromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// encCompletionTokensDetails carries reasoning_tokens.
type encCompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}
