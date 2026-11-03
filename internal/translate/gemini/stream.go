package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/tubeof/proxyllm/internal/translate"
	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/sse"
)

// streamDecoder decodes the Gemini alt=sse stream (pinned via the golden
// fixture) into canonical IR events.
//
// Each data: frame is one COMPLETE GenerateContentResponse chunk — the wire
// has no block framing events, so the machine tracks the currently open block
// kind and emits BlockStop/BlockStart when the payload kind (text, thought,
// functionCall) changes, the same implicit-framing discipline the Ollama
// NDJSON machine uses:
//
//	first chunk → Start (model from modelVersion, prompt usage when present)
//	thought part → BlockStart(thinking) + Delta(thinking)
//	text part → BlockStart(text) + Delta(text)
//	functionCall part → BlockStart(tool_use) + Delta(tool_input, complete)
//	thoughtSignature → Delta(signature) on the open block
//	finishReason + usageMetadata → BlockStop + MessageDelta + Done
//	promptFeedback.blockReason → terminal MessageDelta(content_filter) + Done
//	{"error":…} chunk → Error (terminal)
type streamDecoder struct {
	reader *sse.Reader
	// queue holds canonical events decoded but not yet returned (one chunk
	// can map to several events).
	queue []ir.StreamEvent
	// openKind is the payload kind of the currently open block:
	// "", "text", "thinking", "tool". openIndex is its IR index.
	openKind    string
	openIndex   int
	blockCnt    int
	started     bool
	done        bool
	sawToolCall bool
	// lastUsage holds the newest chunk's usageMetadata — the terminal chunk
	// carries it in practice, but the MessageDelta never emits zero-invented
	// counters when an earlier chunk already reported tokens.
	lastUsage *wireUsageMetadata
}

func newStreamDecoder(r io.Reader) translate.StreamDecoder {
	return &streamDecoder{reader: sse.NewReader(r)}
}

// Read returns the next canonical event. It returns io.EOF after Done (or a
// terminal error) and at clean stream end.
func (d *streamDecoder) Read(_ context.Context) (ir.StreamEvent, error) {
	return d.next()
}

// next pops queued events; when the queue is empty it reads SSE events until
// one produces canonical output or the stream ends.
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
		ev, err := d.reader.Next()
		if err == io.EOF {
			return ir.StreamEvent{}, io.EOF
		}
		if err != nil {
			return ir.StreamEvent{}, fmt.Errorf("gemini: stream read: %w", err)
		}
		if ev.IsComment {
			d.queue = append(d.queue, ir.StreamEvent{Type: ir.Comment})
			continue
		}
		var w wireResponse
		if err := json.Unmarshal([]byte(ev.Data), &w); err != nil {
			return ir.StreamEvent{}, fmt.Errorf("gemini: decode stream chunk: %w", err)
		}
		d.mapChunk(&w)
	}
}

// mapChunk appends the canonical events for one GenerateContentResponse chunk
// to the queue.
func (d *streamDecoder) mapChunk(w *wireResponse) {
	if w.Error != nil {
		msg := w.Error.Message
		if msg == "" {
			msg = w.Error.Status
		}
		if msg == "" {
			msg = "upstream error"
		}
		if !d.started {
			d.started = true
			d.queue = append(d.queue, ir.StreamEvent{
				Type: ir.Start, Response: &ir.Response{Model: w.ModelVersion},
			})
		}
		d.closeBlock()
		d.queue = append(d.queue, ir.StreamEvent{Type: ir.Error,
			Error: &ir.UpstreamError{Type: w.Error.Status, Message: msg}})
		d.done = true
		return
	}
	if w.UsageMetadata != nil {
		d.lastUsage = w.UsageMetadata
	}
	if !d.started {
		d.started = true
		resp := ir.Response{Model: w.ModelVersion}
		if w.UsageMetadata != nil {
			resp.Usage = usageFromWire(w.UsageMetadata)
		}
		d.queue = append(d.queue, ir.StreamEvent{Type: ir.Start, Response: &resp})
	}
	if len(w.Candidates) == 0 {
		if w.PromptFeedback != nil && w.PromptFeedback.BlockReason != "" {
			// "If set, the prompt was blocked and no candidates are
			// returned." The block reason is the stream's honest terminal.
			d.terminal(ir.MessageDeltaPayload{
				StopReason: ir.StopContentFilter,
				Usage:      d.currentUsage(),
			})
		}
		return
	}
	cand := w.Candidates[0]
	d.mapParts(cand.Parts())
	if cand.FinishReason != "" {
		stop := stopReasonFromWire(cand.FinishReason)
		if stop == "" {
			// An unmapped reason still must terminate the stream honestly.
			stop = ir.StopEndTurn
		}
		if stop == ir.StopEndTurn && (d.sawToolCall || candidateHasCall(cand)) {
			// The wire stops a tool-call turn with STOP; the IR names it
			// honestly (the call may have arrived on an earlier chunk).
			stop = ir.StopToolUse
		}
		// candidateCount > 1 is a documented degradation: candidates[0] is
		// served, the rest drop.
		d.terminal(ir.MessageDeltaPayload{StopReason: stop, Usage: d.currentUsage()})
	}
}

// candidateHasCall reports whether one candidate carries a functionCall part.
func candidateHasCall(cand wireCandidate) bool {
	for _, p := range cand.Parts() {
		if p.FunctionCall != nil {
			return true
		}
	}
	return false
}

// mapParts maps one chunk's parts onto block transitions and deltas, keeping
// the wire part order.
func (d *streamDecoder) mapParts(parts []wirePart) {
	for _, p := range parts {
		switch {
		case p.Thought:
			d.openBlock("thinking", ir.ThinkingPart{})
			d.queue = append(d.queue, ir.StreamEvent{Type: ir.Delta, Index: d.openIndex,
				Delta: ir.DeltaPayload{Kind: ir.DeltaThinking, Text: p.Text}})
			if p.ThoughtSignature != "" {
				d.emitSignature(p.ThoughtSignature)
			}
		case p.FunctionCall != nil:
			d.sawToolCall = true
			args := p.FunctionCall.Args
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			d.openBlock("tool", ir.ToolCallPart{ID: p.FunctionCall.ID, Name: p.FunctionCall.Name})
			// Gemini delivers function arguments complete per chunk; they
			// arrive as one fragment, not assembled wire deltas.
			d.queue = append(d.queue, ir.StreamEvent{Type: ir.Delta, Index: d.openIndex,
				Delta: ir.DeltaPayload{Kind: ir.DeltaToolInput, PartialJSON: string(args)}})
			if p.ThoughtSignature != "" {
				d.emitSignature(p.ThoughtSignature)
			}
		case p.InlineData != nil || p.FileData != nil:
			// Media parts have no streamed delta channel; they ride the
			// non-stream body (documented degradation).
		case p.Text != "":
			d.openBlock("text", ir.TextPart{})
			d.queue = append(d.queue, ir.StreamEvent{Type: ir.Delta, Index: d.openIndex,
				Delta: ir.DeltaPayload{Kind: ir.DeltaText, Text: p.Text}})
			if p.ThoughtSignature != "" {
				d.emitSignature(p.ThoughtSignature)
			}
		case p.ThoughtSignature != "":
			d.emitSignature(p.ThoughtSignature)
		default:
			// Unknown part kinds have no streamed channel.
		}
	}
}

// emitSignature emits a signature delta on the open block; with nothing open
// a thinking block opens for it (signatures decorate thought material).
func (d *streamDecoder) emitSignature(sig string) {
	if d.openKind == "" {
		d.openBlock("thinking", ir.ThinkingPart{})
	}
	d.queue = append(d.queue, ir.StreamEvent{Type: ir.Delta, Index: d.openIndex,
		Delta: ir.DeltaPayload{Kind: ir.DeltaSignature, Text: sig}})
}

// currentUsage prefers this chunk's own usageMetadata over the last seen one.
func (d *streamDecoder) currentUsage() ir.Usage {
	// mapChunk's caller stores every chunk's usage; the terminal payload uses
	// whichever is fresher — lastUsage IS the terminal chunk's copy here
	// (mapChunk stores before dispatching).
	return usageFromWire(d.lastUsage)
}

// terminal closes the open block and emits MessageDelta + Done.
func (d *streamDecoder) terminal(md ir.MessageDeltaPayload) {
	d.closeBlock()
	d.queue = append(d.queue,
		ir.StreamEvent{Type: ir.MessageDelta, MessageDelta: &md},
		ir.StreamEvent{Type: ir.Done},
	)
	d.done = true
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
