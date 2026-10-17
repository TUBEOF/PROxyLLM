package codex

// The ChatGPT/Codex backend has no non-streaming mode: a body without
// stream:true is answered 400 {"detail":"Stream must be set to true"}
// (observed live 2026-10-17; the codex CLI always sets it — fixture
// openai-codex/responses-request.json). Provider.Complete therefore
// dispatches the streamed round trip, and this file assembles the canonical
// events into the single ir.Response the non-streaming contract promises.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// assembledBlock is one content block while the stream is open. Deltas are
// kind-checked before accumulating, so a misordered event cannot
// cross-contaminate a block (a signature fragment never lands in text, a
// tool-input fragment never lands in a thinking part).
type assembledBlock struct {
	part      ir.Part
	text      strings.Builder
	signature strings.Builder
	toolInput strings.Builder
}

// assembleResponse drains the canonical event stream into one response.
// Events follow the decoder's ordering contract (Start → (BlockStart →
// Delta* → BlockStop)* → MessageDelta → Done, with Error terminating);
// blocks finalize in index order. An empty text/thinking block is dropped —
// the non-streaming Responses decode emits no part for it either — and a
// tool call whose arguments never arrived carries {}, never an empty
// fragment.
func assembleResponse(ctx context.Context, events <-chan ir.StreamEvent) (ir.Response, error) {
	var (
		resp   ir.Response
		blocks = make(map[int]*assembledBlock)
		order  []int
	)
	var terminalErr *ir.UpstreamError
	terminated := false

	for ev := range events {
		switch ev.Type {
		case ir.Start:
			if ev.Response != nil {
				// Usage stays zero here; it rides the terminal MessageDelta.
				resp.ID, resp.Model = ev.Response.ID, ev.Response.Model
			}
		case ir.BlockStart:
			blocks[ev.Index] = &assembledBlock{part: ev.Part}
			order = append(order, ev.Index)
		case ir.Delta:
			b := blocks[ev.Index]
			if b == nil {
				continue // a delta for an index the stream never opened
			}
			switch ev.Delta.Kind {
			case ir.DeltaText:
				if _, ok := b.part.(ir.TextPart); ok {
					b.text.WriteString(ev.Delta.Text)
				}
			case ir.DeltaThinking:
				if _, ok := b.part.(ir.ThinkingPart); ok {
					b.text.WriteString(ev.Delta.Text)
				}
			case ir.DeltaSignature:
				if _, ok := b.part.(ir.ThinkingPart); ok {
					b.signature.WriteString(ev.Delta.Text)
				}
			case ir.DeltaToolInput:
				if _, ok := b.part.(ir.ToolCallPart); ok {
					b.toolInput.WriteString(ev.Delta.PartialJSON)
				}
			}
		case ir.BlockStop:
			// The block stays addressable (a late delta is still its delta)
			// and finalizes below. A duplicate stop is a no-op by
			// construction: the decoder emits one for the previous block
			// when an item type it does not open a block for ends.
		case ir.MessageDelta:
			if ev.MessageDelta != nil {
				resp.StopReason = ev.MessageDelta.StopReason
				resp.StopSequence = ev.MessageDelta.StopSequence
				resp.Usage = ev.MessageDelta.Usage
			}
		case ir.Error:
			terminalErr = ev.Error
			if terminalErr == nil {
				terminalErr = &ir.UpstreamError{Message: "upstream reported an error"}
			}
		case ir.Done:
			terminated = true
		case ir.Ping, ir.Comment:
			// Keepalives carry nothing for a non-streaming answer.
		}
	}

	if terminalErr != nil {
		return ir.Response{}, &provider.UpstreamError{
			Type:    terminalErr.Type,
			Code:    terminalErr.Code,
			Message: terminalErr.Message,
		}
	}
	if !terminated {
		// The pump synthesizes an Error event when an upstream stream ends
		// without its terminal marker, so this is either a consumer
		// cancellation or a defensive fallback.
		if err := ctx.Err(); err != nil {
			// The caller walked away or a budget of ours ran out. Return the
			// context error unwrapped in an upstream error: routing must
			// classify it as a client abort, never as evidence about the
			// account.
			return ir.Response{}, err
		}
		return ir.Response{}, fmt.Errorf("codex: stream ended without a terminal event")
	}

	for _, idx := range order {
		b := blocks[idx]
		switch p := b.part.(type) {
		case ir.TextPart:
			if b.text.Len() > 0 {
				resp.Content = append(resp.Content, ir.TextPart{Text: b.text.String(), CacheControl: p.CacheControl})
			}
		case ir.ThinkingPart:
			// Signature stays empty when the stream carried none —
			// encrypted_content is not streamed, and a signature is never
			// fabricated.
			if b.text.Len() > 0 || b.signature.Len() > 0 {
				resp.Content = append(resp.Content, ir.ThinkingPart{
					Thinking:  b.text.String(),
					Signature: b.signature.String(),
				})
			}
		case ir.ToolCallPart:
			input := b.toolInput.String()
			if input == "" {
				input = "{}"
			}
			resp.Content = append(resp.Content, ir.ToolCallPart{
				ID:    p.ID,
				Name:  p.Name,
				Input: json.RawMessage(input),
			})
		}
	}
	return resp, nil
}
