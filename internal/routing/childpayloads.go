package routing

// Per-sub-operation payloads (2026-10-10 Logs page): a sub-operation's own
// detail view needs what THAT step sent and received — the tool call and its
// result, the instruction a describer received and the caption it produced.
// The parent request's capture already shows the conversation and the answer,
// but it cannot answer "what did the fallback step actually do": the caption
// text and the tool result exist only transiently inside the routing code.
//
// Two shape decisions:
//
//   - **Facts, not display JSON.** The recorder stores what happened; the write
//     path (httpapi) renders it, exactly as it renders the parent capture. A
//     renderer here would put a WebUI concern in the routing package.
//   - **Off the trace's wire schema.** The payloads live on the in-memory Trace
//     but are never marshalled into route_trace (see Trace.payloads). The
//     versioned trace stays metadata, so no route_trace version bump.

// ChildPayload is what one sub-operation sent and received.
type ChildPayload struct {
	// Kind matches the ChildOp it belongs to (routing.ChildOps), which is how
	// the write path pairs a payload with the row it persisted.
	Kind OpKind

	// ProviderID/Model identify the step, mirroring the ChildOp.
	ProviderID string
	Model      string

	// Tool is the emulated tool name (web_search / web_fetch).
	Tool string

	// Input is the step's own input: a tool call's arguments JSON, or the
	// instruction a describer received.
	Input string

	// Output is what the step produced: the result payload handed back to the
	// model (already the wire-shaped JSON it saw), or the caption text.
	Output string

	// MediaType/ImageBytes describe the image a describer was given — the same
	// rule the parent capture follows: an image is recorded by its type and
	// size, never its bytes.
	MediaType  string
	ImageBytes int
}

// addPayload appends one payload. Same mutex as the events: the emulation loop
// and a live-stream writer can touch the Trace concurrently.
func (t *Trace) addPayload(p ChildPayload) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.payloads = append(t.payloads, p)
}

// EmulatedToolPayload records one emulated tool call: the tool, the arguments
// the model supplied, and the result payload it was handed back.
func (t *Trace) EmulatedToolPayload(tool, input, result string) {
	t.addPayload(ChildPayload{Kind: OpEmulatedTool, Tool: tool, Input: input, Output: result})
}

// CaptionPayload records one image description: the instruction the describer
// received, the image it was given (media type and decoded size only) and the
// text it produced.
func (t *Trace) CaptionPayload(providerID, model, instruction, mediaType string, imageBytes int, caption string) {
	t.addPayload(ChildPayload{
		Kind:       OpImageCaption,
		ProviderID: providerID,
		Model:      model,
		Input:      instruction,
		Output:     caption,
		MediaType:  mediaType,
		ImageBytes: imageBytes,
	})
}
