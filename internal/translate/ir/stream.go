package ir

// StreamEvent is the canonical streaming event IR. Protocol
// SSE machines emit/decode these; the hub never sees wire formats.
//
// Ordering contract (the Anthropic stream order is the reference discipline;
// other protocols map onto it):
//
//	Start → (BlockStart → Delta* → BlockStop)* → MessageDelta → Done
//	(ping anywhere; Error terminates the stream)
type StreamEvent struct {
	Type StreamEventType `json:"type"`

	// Index is the content-block index (BlockStart/Delta/BlockStop).
	Index int `json:"index,omitempty"`

	// Part carries the initial block content on BlockStart (e.g. an empty
	// tool_use with ID+name, or an empty text/thinking part).
	Part Part `json:"-"`

	// Response is the message snapshot on Start (id, model, input usage).
	Response *Response `json:"response,omitempty"`

	// Delta is the incremental content for Delta events.
	Delta DeltaPayload `json:"delta,omitempty"`

	// MessageDelta carries terminal StopReason + final Usage.
	MessageDelta *MessageDeltaPayload `json:"message_delta,omitempty"`

	// Error carries the terminal error.
	Error *UpstreamError `json:"error,omitempty"`
}

// StreamEventType enumerates canonical event types.
type StreamEventType string

const (
	// Start is message_start: message snapshot with model, role, usage
	// (input tokens; output 0) in Response.
	Start StreamEventType = "start"

	// BlockStart opens a content block at Index with initial Part.
	BlockStart StreamEventType = "block_start"

	// Delta appends incremental content to the block at Index.
	Delta StreamEventType = "delta"

	// BlockStop closes the block at Index.
	BlockStop StreamEventType = "block_stop"

	// MessageDelta is message_delta: terminal stop reason + usage.
	MessageDelta StreamEventType = "message_delta"

	// Done is message_stop.
	Done StreamEventType = "done"

	// Ping is a keepalive (relayed, counts toward the silence watchdog).
	Ping StreamEventType = "ping"

	// Error terminates the stream with a protocol-conform error.
	Error StreamEventType = "error"

	// Comment is an SSE comment line (relayed verbatim, including any
	// keepalive frame an upstream sends).
	Comment StreamEventType = "comment"
)

// DeltaKind discriminates Delta payloads.
type DeltaKind string

const (
	// DeltaText is incremental text content.
	DeltaText DeltaKind = "text"

	// DeltaThinking is incremental thinking content (Anthropic's
	// thinking_delta).
	DeltaThinking DeltaKind = "thinking"

	// DeltaSignature is a thinking-block signature fragment (Anthropic's
	// signature_delta, arrives immediately before the block stop).
	DeltaSignature DeltaKind = "signature"

	// DeltaToolInput is a fragment of a tool call's JSON input
	// (Anthropic's input_json_delta; OpenAI argument fragments).
	DeltaToolInput DeltaKind = "tool_input"
)

// DeltaPayload is the incremental payload of a Delta event.
type DeltaPayload struct {
	Kind DeltaKind `json:"kind"`

	// Text is the fragment for DeltaText / DeltaThinking / DeltaSignature.
	Text string `json:"text,omitempty"`

	// ToolIndex disambiguates parallel tool calls within one block
	// (OpenAI assembles by tool_calls index; Anthropic emits one
	// tool_use block per call, so ToolIndex is always 0 there).
	ToolIndex int `json:"tool_index,omitempty"`

	// PartialJSON is the raw JSON fragment for DeltaToolInput.
	PartialJSON string `json:"partial_json,omitempty"`
}

// MessageDeltaPayload is the terminal MessageDelta/Done payload.
type MessageDeltaPayload struct {
	StopReason   StopReason `json:"stop_reason"`
	StopSequence string     `json:"stop_sequence,omitempty"`
	Usage        Usage      `json:"usage"`
}
