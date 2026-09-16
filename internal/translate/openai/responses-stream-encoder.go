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

// responsesStreamEncoder encodes canonical IR events into the OpenAI
// Responses-API SSE wire format (facts O5, O7;
// developers.openai.com/api/docs/api-reference/responses-streaming, accessed
// 2026-09-10): named SSE events (`event:` + one `data:` line pair), unlike
// the chat-completions data-only wire. Wire conformance is pinned by the
// golden fixture testdata/golden/openai/responses-stream.sse and enforced via
// round trips through the fixture-verified Responses stream decoder.
//
// The encoder is a pure relay: it mints wire item ids and tracks open output
// items so delta events carry the correct item_id + output_index, but it
// accumulates NO content — content assembly is the DECODER's job, never the
// encoder's (A6 discipline).
//
// Mapping (the exact inverse of responsesStreamDecoder.mapEvent):
//   - Start → response.created with a FULL response snapshot in .response
//     (id, object:"response", status:"in_progress", model, created_at,
//     usage:null; the IR carries only id+model+usage, so the snapshot's
//     extra fields are sensible defaults — the decoder consumes only
//     id/model/usage from it)
//   - BlockStart(text) → response.output_item.added
//     {item:{id:"msg_n", type:"message", status:"in_progress",
//     role:"assistant", content:[]}}
//   - BlockStart(tool_use) → response.output_item.added
//     {item:{id:"fc_n", type:"function_call", call_id, name}} (O7:
//     function_call items are FLAT — call_id + name at the item level, no
//     nested function wrapper)
//   - BlockStart(thinking) → response.output_item.added
//     {item:{id:"rs_n", type:"reasoning"}} (O7)
//   - Delta(text) → response.output_text.delta
//     {item_id, output_index, content_index:0, delta}
//   - Delta(thinking) → response.reasoning_summary_text.delta
//     {item_id, output_index, summary_index:0, delta} (O7)
//   - Delta(tool_input) → response.function_call_arguments.delta
//     {item_id, output_index, delta} (O7)
//   - BlockStop → response.output_item.done with the completed item
//   - MessageDelta → response.completed with the FULL final snapshot:
//     status:"completed", usage mapped from IR
//     {input_tokens, output_tokens, output_tokens_details:{reasoning_tokens},
//     total_tokens}, and an output array mirroring the emitted items (the
//     encoder accumulates just enough item metadata — id/type/status — to
//     reproduce the fixture's output array shape; text content is NOT
//     reassembled, it carries the concatenated deltas for message items since
//     the fixture's completed snapshot includes the full text)
//   - Done → no-op (response.completed is the terminal event; the fixture
//     stream ends there — Done carries nothing the wire doesn't already have)
//   - Error → event: error with FLAT {code, message, param} shape (O7 —
//     Responses error events are FLAT, unlike chat's nested error object)
//   - Ping/Comment/unknown kinds → a wire comment line (A5 discipline: never
//     fail the stream on unknown kinds)
//
// Each Write flushes when the sink implements http.Flusher (C2: relay
// as-it-arrives).

// responsesStreamEncoderState is the state machine for the encoder. It is
// split out so the state is visible in the type (and testable in isolation).
type responsesStreamEncoder struct {
	w io.Writer

	// Response id/model captured from Start; appear on every snapshot.
	id    string
	model string

	// createdAt is the Unix timestamp minted on Start (the wire timestamp of
	// this response; the decoder does not consume it semantically).
	createdAt int64

	// openItems tracks the wire output items opened by BlockStart and not yet
	// closed by BlockStop. The index into this slice is the IR block index;
	// each entry carries the minted wire item id + the item kind + accumulated
	// text (for message items, to reproduce the completed snapshot's content).
	openItems []*responsesEncItem

	// completedItems holds items that have been closed by BlockStop, for the
	// final response.completed output array.
	completedItems []*responsesEncItem

	// itemCounter mints unique wire item ids (msg_0, fc_0, rs_0, ...).
	itemCounter int
}

// responsesEncItem is one tracked output item on the encoder side.
type responsesEncItem struct {
	wireID   string // minted wire item id (msg_n / fc_n / rs_n)
	kind     string // "message" | "function_call" | "reasoning"
	callID   string // function_call only
	name     string // function_call only
	text     string // accumulated text for message/reasoning items
	outputIx int    // output_index on the wire
}

// NewResponsesStreamEncoder returns the OpenAI Responses-API SSE stream
// encoder for w.
func NewResponsesStreamEncoder(w io.Writer) translate.StreamEncoder {
	return &responsesStreamEncoder{w: w}
}

// Write encodes one canonical event onto the wire.
func (e *responsesStreamEncoder) Write(ctx context.Context, ev ir.StreamEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch ev.Type {
	case ir.Start:
		if ev.Response != nil {
			e.id = ev.Response.ID
			e.model = ev.Response.Model
		}
		if e.createdAt == 0 {
			e.createdAt = time.Now().Unix()
		}
		return e.emitEvent("response.created", e.createdSnapshot())
	case ir.BlockStart:
		return e.emitBlockStart(ev)
	case ir.Delta:
		return e.emitDelta(ev)
	case ir.BlockStop:
		return e.emitBlockStop(ev)
	case ir.MessageDelta:
		return e.emitCompleted(ev)
	case ir.Done:
		// response.completed is the terminal event; Done is a no-op (the
		// fixture stream ends with response.completed as the final event).
		return nil
	case ir.Error:
		return e.emitError(ev)
	case ir.Ping, ir.Comment:
		return e.emitComment("")
	default:
		// Unknown event kinds relay as wire comments (A5 discipline).
		return e.emitComment("")
	}
}

// emitBlockStart maps a BlockStart onto response.output_item.added.
func (e *responsesStreamEncoder) emitBlockStart(ev ir.StreamEvent) error {
	ix := e.itemCounter
	e.itemCounter++
	item := &responsesEncItem{
		outputIx: len(e.openItems),
	}
	switch p := ev.Part.(type) {
	case ir.TextPart:
		item.wireID = fmt.Sprintf("msg_%d", ix)
		item.kind = "message"
		e.openItems = append(e.openItems, item)
		return e.emitEvent("response.output_item.added", e.outputItemAddedMessage(item))
	case ir.ToolCallPart:
		item.wireID = fmt.Sprintf("fc_%d", ix)
		item.kind = "function_call"
		item.callID = p.ID
		item.name = p.Name
		e.openItems = append(e.openItems, item)
		return e.emitEvent("response.output_item.added", e.outputItemAddedFunctionCall(item))
	case ir.ThinkingPart:
		item.wireID = fmt.Sprintf("rs_%d", ix)
		item.kind = "reasoning"
		e.openItems = append(e.openItems, item)
		return e.emitEvent("response.output_item.added", e.outputItemAddedReasoning(item))
	default:
		// Unknown part kind: relay as comment (A5).
		return e.emitComment("")
	}
}

// emitDelta maps a Delta onto the appropriate named delta event.
func (e *responsesStreamEncoder) emitDelta(ev ir.StreamEvent) error {
	if ev.Index < 0 || ev.Index >= len(e.openItems) {
		// No open item for this index; drop (A5).
		return e.emitComment("")
	}
	item := e.openItems[ev.Index]
	if item == nil {
		// The slot was closed by BlockStop (which leaves a nil
		// placeholder so indices stay stable); a delta for a closed item
		// must not panic on malformed upstream interleavings.
		return e.emitComment("")
	}
	switch ev.Delta.Kind {
	case ir.DeltaText:
		item.text += ev.Delta.Text
		return e.emitEvent("response.output_text.delta",
			e.textDelta(item, ev.Delta.Text))
	case ir.DeltaThinking:
		item.text += ev.Delta.Text
		return e.emitEvent("response.reasoning_summary_text.delta",
			e.reasoningDelta(item, ev.Delta.Text))
	case ir.DeltaToolInput:
		// Accumulate for the completed snapshot's arguments (function_call
		// items are FLAT, O7): the completed snapshot carries the FULL
		// assembled arguments string, so fragments must be collected like
		// DeltaText — arguments:"" would make a client re-issuing the tool
		// call from the snapshot call the tool with no arguments.
		item.text += ev.Delta.PartialJSON
		return e.emitEvent("response.function_call_arguments.delta",
			e.toolInputDelta(item, ev.Delta.PartialJSON))
	case ir.DeltaSignature:
		// No dedicated wire channel for reasoning signatures in the stream;
		// drop (A5, like the chat encoder's thinking/signature degradation).
		return nil
	default:
		return e.emitComment("")
	}
}

// emitBlockStop maps a BlockStop onto response.output_item.done.
func (e *responsesStreamEncoder) emitBlockStop(ev ir.StreamEvent) error {
	if ev.Index < 0 || ev.Index >= len(e.openItems) {
		return e.emitComment("")
	}
	item := e.openItems[ev.Index]
	// Move to completed for the final output array.
	e.completedItems = append(e.completedItems, item)
	// Close the slot; leave a nil placeholder so indices stay stable.
	e.openItems[ev.Index] = nil
	return e.emitEvent("response.output_item.done", e.outputItemDone(item))
}

// emitCompleted maps a MessageDelta onto response.completed with the full
// final snapshot.
func (e *responsesStreamEncoder) emitCompleted(ev ir.StreamEvent) error {
	var md *ir.MessageDeltaPayload
	if ev.MessageDelta != nil {
		md = ev.MessageDelta
	}
	var u ir.Usage
	if md != nil {
		u = md.Usage
	}
	return e.emitEvent("response.completed", e.completedSnapshot(u))
}

// ---- snapshot builders ----

// createdSnapshot builds the response.created payload: full snapshot with
// status in_progress and usage null.
func (e *responsesStreamEncoder) createdSnapshot() string {
	resp := e.baseSnapshot("in_progress")
	resp.Usage = nil // response.created carries usage:null (fixture line 2)
	resp.Output = []responsesWireItem{}
	raw, _ := json.Marshal(struct {
		Type     string           `json:"type"`
		Response *respEncSnapshot `json:"response"`
	}{
		Type:     "response.created",
		Response: resp,
	})
	return string(raw)
}

// completedSnapshot builds the response.completed payload: full snapshot with
// status completed, usage from IR, and output array mirroring emitted items.
func (e *responsesStreamEncoder) completedSnapshot(u ir.Usage) string {
	resp := e.baseSnapshot("completed")
	resp.Usage = &responsesWireUsage{
		InputTokens:         u.InputTokens,
		OutputTokens:        u.OutputTokens,
		TotalTokens:         u.InputTokens + u.OutputTokens,
		OutputTokensDetails: &responsesWireUsageDetails{ReasoningTokens: u.ReasoningTokens},
	}
	resp.Output = e.buildCompletedOutput()
	raw, _ := json.Marshal(struct {
		Type     string           `json:"type"`
		Response *respEncSnapshot `json:"response"`
	}{
		Type:     "response.completed",
		Response: resp,
	})
	return string(raw)
}

// buildCompletedOutput assembles the output array for response.completed from
// the tracked completed items. Message items carry their accumulated text as
// an output_text part (mirroring the fixture line 29).
func (e *responsesStreamEncoder) buildCompletedOutput() []responsesWireItem {
	out := make([]responsesWireItem, 0, len(e.completedItems))
	for _, item := range e.completedItems {
		switch item.kind {
		case "message":
			out = append(out, responsesWireItem{
				ID:     item.wireID,
				Type:   "message",
				Status: "completed",
				Role:   "assistant",
				Content: &responsesWireContent{
					Parts: []responsesWirePart{
						{Type: "output_text", Text: item.text},
					},
				},
			})
		case "function_call":
			out = append(out, responsesWireItem{
				ID:        item.wireID,
				Type:      "function_call",
				Status:    "completed",
				CallID:    item.callID,
				Name:      item.name,
				Arguments: item.text,
			})
		case "reasoning":
			out = append(out, responsesWireItem{
				ID:     item.wireID,
				Type:   "reasoning",
				Status: "completed",
			})
		}
	}
	return out
}

// baseSnapshot is the shared response snapshot skeleton for created/completed.
// The fields mirror the golden fixture's field set (lines 2, 29); the IR does
// not carry instructions/temperature/etc., so those are sensible defaults.
func (e *responsesStreamEncoder) baseSnapshot(status string) *respEncSnapshot {
	return &respEncSnapshot{
		ID:                 e.id,
		Object:             "response",
		CreatedAt:          e.createdAt,
		Status:             status,
		Error:              nil,
		IncompleteDetails:  nil,
		Instructions:       "",
		MaxOutputTokens:    nil,
		Model:              e.model,
		Output:             []responsesWireItem{},
		ParallelToolCalls:  true,
		PreviousResponseID: nil,
		Reasoning:          respEncReasoning{},
		Store:              true,
		Temperature:        1.0,
		Text:               respEncText{Format: respEncTextFormat{Type: "text"}},
		ToolChoice:         "auto",
		Tools:              []json.RawMessage{},
		TopP:               1.0,
		Truncation:         "disabled",
		User:               nil,
		Metadata:           map[string]any{},
	}
}

// respEncSnapshot is the encoder-side response snapshot, mirroring the
// fixture's field set and order. Unlike the decoder's responsesWireResponse
// (which tolerates absent fields), this struct reproduces the exact wire
// shape the fixture carries.
type respEncSnapshot struct {
	ID                 string              `json:"id"`
	Object             string              `json:"object"`
	CreatedAt          int64               `json:"created_at"`
	Status             string              `json:"status"`
	Error              *wireAPIError       `json:"error"`
	IncompleteDetails  *json.RawMessage    `json:"incomplete_details"`
	Instructions       string              `json:"instructions"`
	MaxOutputTokens    *int                `json:"max_output_tokens"`
	Model              string              `json:"model"`
	Output             []responsesWireItem `json:"output"`
	ParallelToolCalls  bool                `json:"parallel_tool_calls"`
	PreviousResponseID *string             `json:"previous_response_id"`
	Reasoning          respEncReasoning    `json:"reasoning"`
	Store              bool                `json:"store"`
	Temperature        float64             `json:"temperature"`
	Text               respEncText         `json:"text"`
	ToolChoice         string              `json:"tool_choice"`
	Tools              []json.RawMessage   `json:"tools"`
	TopP               float64             `json:"top_p"`
	Truncation         string              `json:"truncation"`
	Usage              *responsesWireUsage `json:"usage"`
	User               *string             `json:"user"`
	Metadata           map[string]any      `json:"metadata"`
}

type respEncReasoning struct {
	Effort  *string `json:"effort"`
	Summary *string `json:"summary"`
}

type respEncText struct {
	Format respEncTextFormat `json:"format"`
}

type respEncTextFormat struct {
	Type string `json:"type"`
}

// ---- event payload builders ----

func (e *responsesStreamEncoder) outputItemAddedMessage(item *responsesEncItem) string {
	raw, _ := json.Marshal(struct {
		Type        string            `json:"type"`
		OutputIndex int               `json:"output_index"`
		Item        responsesWireItem `json:"item"`
	}{
		Type:        "response.output_item.added",
		OutputIndex: item.outputIx,
		Item: responsesWireItem{
			ID:      item.wireID,
			Type:    "message",
			Status:  "in_progress",
			Role:    "assistant",
			Content: &responsesWireContent{Parts: []responsesWirePart{}},
		},
	})
	return string(raw)
}

func (e *responsesStreamEncoder) outputItemAddedFunctionCall(item *responsesEncItem) string {
	raw, _ := json.Marshal(struct {
		Type        string            `json:"type"`
		OutputIndex int               `json:"output_index"`
		Item        responsesWireItem `json:"item"`
	}{
		Type:        "response.output_item.added",
		OutputIndex: item.outputIx,
		Item: responsesWireItem{
			ID:     item.wireID,
			Type:   "function_call",
			CallID: item.callID,
			Name:   item.name,
		},
	})
	return string(raw)
}

func (e *responsesStreamEncoder) outputItemAddedReasoning(item *responsesEncItem) string {
	raw, _ := json.Marshal(struct {
		Type        string            `json:"type"`
		OutputIndex int               `json:"output_index"`
		Item        responsesWireItem `json:"item"`
	}{
		Type:        "response.output_item.added",
		OutputIndex: item.outputIx,
		Item: responsesWireItem{
			ID:   item.wireID,
			Type: "reasoning",
		},
	})
	return string(raw)
}

func (e *responsesStreamEncoder) textDelta(item *responsesEncItem, text string) string {
	raw, _ := json.Marshal(struct {
		Type         string `json:"type"`
		ItemID       string `json:"item_id"`
		OutputIndex  int    `json:"output_index"`
		ContentIndex int    `json:"content_index"`
		Delta        string `json:"delta"`
	}{
		Type:         "response.output_text.delta",
		ItemID:       item.wireID,
		OutputIndex:  item.outputIx,
		ContentIndex: 0,
		Delta:        text,
	})
	return string(raw)
}

func (e *responsesStreamEncoder) reasoningDelta(item *responsesEncItem, text string) string {
	raw, _ := json.Marshal(struct {
		Type         string `json:"type"`
		ItemID       string `json:"item_id"`
		OutputIndex  int    `json:"output_index"`
		SummaryIndex int    `json:"summary_index"`
		Delta        string `json:"delta"`
	}{
		Type:         "response.reasoning_summary_text.delta",
		ItemID:       item.wireID,
		OutputIndex:  item.outputIx,
		SummaryIndex: 0,
		Delta:        text,
	})
	return string(raw)
}

func (e *responsesStreamEncoder) toolInputDelta(item *responsesEncItem, fragment string) string {
	raw, _ := json.Marshal(struct {
		Type        string `json:"type"`
		ItemID      string `json:"item_id"`
		OutputIndex int    `json:"output_index"`
		Delta       string `json:"delta"`
	}{
		Type:        "response.function_call_arguments.delta",
		ItemID:      item.wireID,
		OutputIndex: item.outputIx,
		Delta:       fragment,
	})
	return string(raw)
}

func (e *responsesStreamEncoder) outputItemDone(item *responsesEncItem) string {
	wireItem := responsesWireItem{
		ID:     item.wireID,
		Type:   item.kind,
		Status: "completed",
	}
	switch item.kind {
	case "message":
		wireItem.Role = "assistant"
		wireItem.Content = &responsesWireContent{
			Parts: []responsesWirePart{
				{Type: "output_text", Text: item.text},
			},
		}
	case "function_call":
		wireItem.CallID = item.callID
		wireItem.Name = item.name
		wireItem.Arguments = item.text
	}
	raw, _ := json.Marshal(struct {
		Type        string            `json:"type"`
		OutputIndex int               `json:"output_index"`
		Item        responsesWireItem `json:"item"`
	}{
		Type:        "response.output_item.done",
		OutputIndex: item.outputIx,
		Item:        wireItem,
	})
	return string(raw)
}

// emitError maps a terminal ir.Error onto the FLAT Responses error event
// (O7: {type, code, message, param} at top level — no nested error object,
// unlike the chat wire).
func (e *responsesStreamEncoder) emitError(ev ir.StreamEvent) error {
	type flatError struct {
		Type    string  `json:"type"`
		Code    string  `json:"code,omitempty"`
		Message string  `json:"message"`
		Param   *string `json:"param"`
	}
	fe := flatError{Type: "error", Param: nil}
	if ev.Error != nil {
		fe.Code = ev.Error.Code
		fe.Message = ev.Error.Message
	}
	if fe.Message == "" {
		fe.Message = "upstream error"
	}
	raw, err := json.Marshal(fe)
	if err != nil {
		return fmt.Errorf("openai/responses: encode error event: %w", err)
	}
	return e.emitEvent("error", string(raw))
}

// ---- wire I/O ----

// emitEvent writes one named SSE event: `event:` + `data:` + blank line,
// flushing (C2). Follows the chat stream-encoder's writeRaw pattern.
func (e *responsesStreamEncoder) emitEvent(name, data string) error {
	frame := "event: " + name + "\ndata: " + data + "\n\n"
	return e.writeRaw(frame)
}

// emitComment writes a wire comment line (SSE-3, A5).
func (e *responsesStreamEncoder) emitComment(text string) error {
	if text != "" {
		text = " " + text
	}
	return e.writeRaw(":" + text + "\n\n")
}

// writeRaw writes one wire frame and flushes (C2).
func (e *responsesStreamEncoder) writeRaw(s string) error {
	if _, err := io.WriteString(e.w, s); err != nil {
		return fmt.Errorf("openai/responses: encode stream: %w", err)
	}
	if f, ok := e.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}
