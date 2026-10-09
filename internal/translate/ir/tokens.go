package ir

// Local input-token estimation. Moved here from httpapi (2026-10-09) so
// both consumers share one source of truth: the count_tokens surface
// (/v1/messages/count_tokens, C1's heuristic marking) and the routing
// executor's capability-fallback context gate, which must compare an
// incoming request against a fallback model's declared window BEFORE
// dispatching a conversation that model cannot hold.
//
// It is deliberately a heuristic, never presented as an exact count: a
// real tokenizer per provider would be a much larger dependency, and the
// gate only needs to know the order of magnitude (is this a 4k turn or a
// 400k conversation?).

// EstimateTokens estimates the request's input tokens as chars/4 over
// system + messages + tool schemas, calibrated against the observed
// ~4 chars/token ratio (Analysis D). Images are not char-countable and
// take a flat per-image allowance (the count_tokens contract, C1) — the
// estimate is therefore a LOWER bound for image-heavy requests, which is
// safe: an under-estimate lets a dispatch through, where the FO-6
// context-window class fails it over instead of terminating.
func (r Request) EstimateTokens() int {
	chars := 0
	for _, sp := range r.System {
		chars += len(sp.Text)
	}
	for _, msg := range r.Messages {
		for _, part := range msg.Parts {
			switch p := part.(type) {
			case TextPart:
				chars += len(p.Text)
			case ToolCallPart:
				chars += len(p.Input)
			case ToolResultPart:
				for _, sub := range p.Content {
					if t, ok := sub.(TextPart); ok {
						chars += len(t.Text)
					}
				}
			case ImagePart:
				chars += 1024
			}
		}
	}
	for _, tool := range r.Tools {
		chars += len(tool.Name) + len(tool.Description) + len(tool.InputSchema)
	}
	return max(chars/4, 1)
}
