package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tubeof/proxyllm/internal/translate"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// streamEncoder encodes canonical IR events into the Gemini alt=sse stream
// wire format (api-reference/generate-content/stream-generate-content,
// accessed 2026-11-03): `data:`-framed JSON chunks, each one complete
// GenerateContentResponse, the final chunk carrying finishReason +
// usageMetadata. Wire conformance is enforced via round trips through the
// package's fixture-verified stream decoder.
//
// Mapping (the inverse of streamDecoder.mapChunk):
//
//   - Start → the framing chunk: a candidateless chunk announcing modelVersion
//     (the wire carries no block-open events; the decoder emits Start from any
//     first chunk)
//   - BlockStart(text/thinking) → nothing (framing is payload-driven)
//   - BlockStart(tool_use) → nothing here; the accumulated function call rides
//     the line of its FIRST argument fragment (below)
//   - Delta(text) → a chunk with one text part
//   - Delta(thinking) → a chunk with one thought part (text + thought: true)
//   - Delta(signature) → a chunk with a bare thoughtSignature part (the part
//     form the re-encode path replays; signatures are never recomputed)
//   - Delta(tool_input) → fragments accumulate; the functionCall chunk is
//     emitted once the assembled arguments form a complete JSON value
//   - BlockStop(tool) → the functionCall chunk if the arguments never
//     assembled (defensive: the call must not be lost); kinds close implicitly
//   - MessageDelta → the terminal chunk: candidates[0].finishReason +
//     usageMetadata (+ modelVersion)
//   - Done → the terminal chunk if MessageDelta did not arrive (the stream
//     requires a final finishReason chunk; without it the canonical decoder
//     reports an un-terminated stream)
//   - Error → a chunk carrying {"error":{message,...}}
//   - Ping/Comment/unknown → nothing (no wire channel; the degradation is a
//     relay-layer concern, never a stream failure)
//
// Each Write flushes when the sink implements http.Flusher, so bytes reach
// the client as they arrive.
type streamEncoder struct {
	w io.Writer

	model string

	// toolID/toolName/toolArgs assemble the open tool block's wire call from
	// IR fragments; toolOpen reports whether a call is pending, toolSent
	// whether its chunk was already emitted (BlockStop must not double-emit).
	toolID   string
	toolName string
	toolArgs strings.Builder
	toolOpen bool
	toolSent bool

	// done reports whether the terminal finishReason chunk was emitted.
	done bool
}

// NewStreamEncoder returns the Gemini alt=sse stream encoder for w.
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
		// Framing chunk: candidates-less, modelVersion (+ the prompt usage
		// when the start snapshot carries one — the wire's first chunk style).
		frame := wireResponse{ModelVersion: e.model}
		if ev.Response != nil {
			u := usageMetadataFromIR(ev.Response.Usage)
			if u.PromptTokenCount > 0 || u.CachedContentTokenCount > 0 {
				frame.UsageMetadata = u
			}
		}
		return e.writeChunk(frame)
	case ir.BlockStart:
		if tc, ok := ev.Part.(ir.ToolCallPart); ok {
			e.toolOpen = true
			e.toolSent = false
			e.toolID = tc.ID
			e.toolName = tc.Name
			e.toolArgs.Reset()
		}
		return nil
	case ir.Delta:
		switch ev.Delta.Kind {
		case ir.DeltaText:
			return e.writeChunk(chunkWithParts(wirePart{Text: ev.Delta.Text}))
		case ir.DeltaThinking:
			return e.writeChunk(chunkWithParts(wirePart{
				Text:    ev.Delta.Text,
				Thought: true,
			}))
		case ir.DeltaSignature:
			// A bare signature part: the exact form the request re-encode
			// path replays verbatim.
			return e.writeChunk(chunkWithParts(wirePart{ThoughtSignature: ev.Delta.Text}))
		case ir.DeltaToolInput:
			if !e.toolOpen {
				return nil // fragment for a tool block we never saw open
			}
			e.toolArgs.WriteString(ev.Delta.PartialJSON)
			// The wire carries exactly one complete functionCall per part;
			// emit only once the assembled arguments are a complete value.
			// json.Marshal validates RawMessage content, so partial JSON
			// must never ride the wire.
			if !json.Valid([]byte(e.toolArgs.String())) {
				return nil
			}
			e.toolSent = true
			return e.emitToolChunk()
		default:
			return nil
		}
	case ir.BlockStop:
		// Implicit framing: kinds close implicitly on the next chunk's
		// payload. Closing the tool block releases the assembled call —
		// defensively emitting it if the accumulated arguments never became
		// complete JSON (a truncated upstream call must not be lost
		// silently; the decoder will surface the invalid arguments).
		var err error
		if e.toolOpen {
			e.toolOpen = false
			if e.toolArgs.Len() > 0 && !e.toolSent {
				err = e.emitToolChunk()
			}
			e.toolSent = false
			e.toolID = ""
			e.toolName = ""
			e.toolArgs.Reset()
		}
		return err
	case ir.MessageDelta:
		return e.writeTerminal(ev.MessageDelta)
	case ir.Done:
		if e.done {
			return nil
		}
		return e.writeTerminal(nil)
	case ir.Error:
		msg := "upstream error"
		status := ""
		if ev.Error != nil {
			if ev.Error.Message != "" {
				msg = ev.Error.Message
			}
			status = ev.Error.Type
		}
		return e.writeChunk(wireResponse{Error: &wireResponseError{Status: status, Message: msg}})
	case ir.Ping, ir.Comment:
		// No SSE wire channel for keepalives on this surface; the stream
		// stays valid.
		return nil
	default:
		// Unknown event kinds stay off the wire.
		return nil
	}
}

// chunkWithParts builds one content-bearing GenerateContentResponse chunk.
func chunkWithParts(parts ...wirePart) wireResponse {
	return wireResponse{Candidates: []wireCandidate{{
		Content: &wireContent{Role: "model", Parts: parts},
	}}}
}

// emitToolChunk writes the assembled functionCall as one chunk. Callers only
// invoke it with complete JSON arguments (or defensively at BlockStop to
// avoid losing a truncated call).
func (e *streamEncoder) emitToolChunk() error {
	args := json.RawMessage(e.toolArgs.String())
	return e.writeChunk(chunkWithParts(wirePart{FunctionCall: &wireFunctionCall{
		ID:   e.toolID,
		Name: e.toolName,
		Args: args,
	}}))
}

// writeTerminal emits the terminal finishReason chunk from the message-delta
// payload (or defaults when Done arrives without one).
func (e *streamEncoder) writeTerminal(md *ir.MessageDeltaPayload) error {
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
	cand := wireCandidate{FinishReason: stopReasonToWire(reason)}
	w := wireResponse{
		ModelVersion:  e.model,
		UsageMetadata: usageMetadataFromIR(u),
		Candidates:    []wireCandidate{cand},
	}
	return e.writeChunk(w)
}

// writeChunk marshals one chunk, writes it with its SSE framing, and flushes.
func (e *streamEncoder) writeChunk(w wireResponse) error {
	raw, err := json.Marshal(w)
	if err != nil {
		return fmt.Errorf("gemini: encode stream chunk: %w", err)
	}
	if _, err := io.WriteString(e.w, "data: "+string(raw)+"\n\n"); err != nil {
		return fmt.Errorf("gemini: encode stream: %w", err)
	}
	if f, ok := e.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}
