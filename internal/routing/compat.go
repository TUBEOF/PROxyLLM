package routing

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// Compat is the per-model compatibility policy (storage: models.compat JSON,
// WebUI "Models & Compatibility" editor). It governs how requests for a
// target model are adapted before dispatch — every application is recorded in
// the Trace, never silent.
type Compat struct {
	// ThinkingHandling governs the thinking parameter and thinking blocks
	// in history: strip / map-to-reasoning-effort / block ("" = strip,
	// the default for non-Claude targets).
	ThinkingHandling string

	// ServerToolEmulation governs the web-tool emulation layer:
	// native / emulate-firecrawl / off ("" = native).
	ServerToolEmulation string

	// MaxTokensClamp caps MaxTokens for targets with lower output limits.
	// It is operator-set only — the proxy never guesses a target's limit.
	// 0 = off.
	MaxTokensClamp int

	// ContextWindow is the model context window (tokens) for capability
	// filtering and the count_tokens heuristic. 0 = unknown.
	ContextWindow int

	// CacheControl governs cache_control markers: forward / drop (""
	// = forward). Forward applies on Anthropic→Anthropic only; drop
	// removes the markers and logs the degradation.
	CacheControl string
}

// ThinkingHandling values.
const (
	// ThinkingStrip removes the thinking parameter and thinking blocks
	// from history — never sends fabricated signatures upstream.
	// Default for non-Claude targets.
	ThinkingStrip = "strip"

	// ThinkingMapToEffort converts the thinking budget to a
	// reasoning-effort hint on the request (ir.ReasoningEffort) where the
	// target supports it (Ollama and OpenAI reasoning-effort endpoints).
	ThinkingMapToEffort = "map-to-reasoning-effort"

	// ThinkingBlock refuses thinking requests with a protocol-conform
	// error (operators opting into strictness).
	ThinkingBlock = "block"
)

// ServerToolEmulation values.
const (
	EmulationNative           = "native"
	EmulationEmulateFirecrawl = "emulate-firecrawl"
	EmulationOff              = "off"
)

// CacheControl values.
const (
	CacheControlForward = "forward"
	CacheControlDrop    = "drop"
)

// ErrThinkingBlocked is the terminal error for the block policy.
var ErrThinkingBlocked = errors.New("routing: thinking requests are blocked by the model's thinking_handling policy")

// ApplyCompat adapts req per the per-model compatibility policy, recording
// every application in a fresh Trace (never silent — each event names the
// setting and its applied value).
func ApplyCompat(c Compat, req ir.Request) (ir.Request, *Trace, error) {
	trace := &Trace{}

	req, err := applyThinking(c, req, trace)
	if err != nil {
		return req, trace, err
	}
	req = applyClamp(c, req, trace)
	req = applyCacheControl(c, req, trace)
	req = applyEmulation(c, req, trace)
	return req, trace, nil
}

// applyThinking enforces the thinking_handling policy. Requests without a
// thinking parameter pass through every setting unchanged and untraced
// (nothing to degrade) — except history thinking blocks under strip/map,
// which are still removed: signatures must never reach a non-thinking
// target.
func applyThinking(c Compat, req ir.Request, trace *Trace) (ir.Request, error) {
	setting := c.ThinkingHandling
	if setting == "" {
		setting = ThinkingStrip // default for unset policies
	}

	hasThinking := req.Thinking != nil && (req.Thinking.Enabled || req.Thinking.Adaptive)

	// Record the CLIENT's intent before the policy rewrites the parameter:
	// every branch below may nil or remap req.Thinking, and the response
	// path needs to know whether thinking blocks were asked for — see
	// applyResponseThinking.
	req.ThinkingRequested = hasThinking

	switch setting {
	case ThinkingBlock:
		if hasThinking {
			trace.CompatPolicy("thinking_handling", ThinkingBlock, "operator policy blocks thinking requests")
			return req, ErrThinkingBlocked
		}
		return req, nil

	case ThinkingMapToEffort:
		if hasThinking {
			budget := 0
			adaptive := false
			if req.Thinking != nil {
				budget = req.Thinking.BudgetTokens
				adaptive = req.Thinking.Adaptive
			}
			effort := ir.EffortFromBudget(budget)
			// Adaptive thinking maps to the target's own default effort
			// ("" — the provider decides), an explicit budget to its tier.
			req.Thinking = nil
			if effort != "" {
				req.ReasoningEffort = effort
				trace.CompatPolicy("thinking_handling", ThinkingMapToEffort,
					"budget "+strconv.Itoa(budget)+" → reasoning_effort "+effort)
			} else if adaptive {
				trace.CompatPolicy("thinking_handling", ThinkingMapToEffort,
					"adaptive thinking → target-default effort")
			}
		}
		// History thinking blocks cannot ride a non-thinking target:
		// mapped requests drop them too.
		req = stripThinkingBlocks(req)
		return req, nil

	default: // strip
		if hasThinking {
			req.Thinking = nil
			trace.CompatPolicy("thinking_handling", ThinkingStrip, "thinking param removed for non-thinking target")
		}
		req = stripThinkingBlocks(req)
		return req, nil
	}
}

// stripThinkingBlocks removes ThinkingPart blocks from message history
// (never upstream: fabricated or replayed signatures must not be sent).
// System blocks carry no thinking parts, so only Messages are walked.
func stripThinkingBlocks(req ir.Request) ir.Request {
	for mi, msg := range req.Messages {
		kept := msg.Parts[:0]
		for _, part := range msg.Parts {
			if _, ok := part.(ir.ThinkingPart); ok {
				continue
			}
			kept = append(kept, part)
		}
		req.Messages[mi].Parts = kept
	}
	return req
}

// ThinkingOutputStrip is the compat_policy value recorded when a target's
// answer carried thinking blocks the client never asked for.
const ThinkingOutputStrip = "strip-output"

// thinkingAllowed reports whether the target's reasoning may surface to the
// client as a thinking block. The reference returns thinking blocks only for
// requests that ENABLED thinking, and a client that asked for none reads the
// first content block as text — Claude Code's web_fetch helper answers "No
// response from model" when that first block is a thinking block, which made
// every WebFetch fail.
//
// The recorded verdict is authoritative because the policy may have consumed
// the parameter (strip/map nil it out); a request that never went through
// ApplyCompat — a direct executor caller, a test — still decides by its own
// parameter.
func thinkingAllowed(req ir.Request) bool {
	if req.ThinkingRequested {
		return true
	}
	return req.Thinking != nil && (req.Thinking.Enabled || req.Thinking.Adaptive)
}

// applyResponseThinking removes thinking blocks from a target's answer when
// the client never enabled thinking, and records the drop (never silent). The
// blocks are the target's own reasoning, leaked into a response shape the
// reference does not produce for that request.
func applyResponseThinking(req ir.Request, resp ir.Response, trace *Trace) ir.Response {
	if thinkingAllowed(req) {
		return resp
	}
	kept := make([]ir.Part, 0, len(resp.Content))
	dropped := 0
	for _, part := range resp.Content {
		if _, isThinking := part.(ir.ThinkingPart); isThinking {
			dropped++
			continue
		}
		kept = append(kept, part)
	}
	if dropped == 0 {
		return resp
	}
	resp.Content = kept
	trace.CompatPolicy("thinking_handling", ThinkingOutputStrip,
		strconv.Itoa(dropped)+" unsolicited thinking block(s) dropped: the request did not enable thinking")
	return resp
}

// applyResponseThinkingStream is the streaming half of applyResponseThinking:
// the failure rides the stream too, so the filter must sit on the events
// (a client assembles its message from them). Dropped blocks are renumbered
// out — the Anthropic encoder relays ev.Index verbatim, so leaving a hole
// would emit a block index the reference never produces.
//
// The passthrough runs on its own goroutine and honors ctx on both sides,
// like addUsageToStream: a caller that stops reading releases it.
func applyResponseThinkingStream(ctx context.Context, req ir.Request, in <-chan ir.StreamEvent, trace *Trace) <-chan ir.StreamEvent {
	if thinkingAllowed(req) {
		return in
	}
	out := make(chan ir.StreamEvent) // unbuffered: backpressure to the caller
	go func() {
		defer close(out)
		dropped := map[int]bool{}
		remap := map[int]int{}
		kept := 0
		reported := false
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-in:
				if !ok {
					return
				}
				switch ev.Type {
				case ir.BlockStart:
					if _, isThinking := ev.Part.(ir.ThinkingPart); isThinking {
						dropped[ev.Index] = true
						if !reported {
							reported = true
							trace.CompatPolicy("thinking_handling", ThinkingOutputStrip,
								"unsolicited thinking block dropped: the request did not enable thinking")
						}
						continue
					}
					if len(dropped) > 0 {
						// A new block may reuse an index a previous block had —
						// the drop verdict belonged to THAT block, so clear it
						// before renumbering, or the new block's deltas and stop
						// would be swallowed by the dropped-index check below.
						delete(dropped, ev.Index)
						remap[ev.Index] = kept
						ev.Index = kept
					}
					kept++
				case ir.Delta, ir.BlockStop:
					if dropped[ev.Index] {
						continue
					}
					if n, ok := remap[ev.Index]; ok {
						ev.Index = n
					}
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// applyClamp caps MaxTokens when the policy sets a lower output limit.
func applyClamp(c Compat, req ir.Request, trace *Trace) ir.Request {
	if c.MaxTokensClamp > 0 && req.MaxTokens > c.MaxTokensClamp {
		trace.CompatPolicy("max_tokens_clamp", strconv.Itoa(c.MaxTokensClamp),
			"max_tokens "+strconv.Itoa(req.MaxTokens)+" clamped to target limit")
		req.MaxTokens = c.MaxTokensClamp
	}
	return req
}

// applyCacheControl drops cache_control markers per the drop policy; the
// degradation is logged via the trace event (forward is the default and
// applies Anthropic→Anthropic only — enforced by the caller choosing
// the policy, not re-derived here).
func applyCacheControl(c Compat, req ir.Request, trace *Trace) ir.Request {
	if c.CacheControl != CacheControlDrop {
		return req
	}
	dropped := false
	for si := range req.System {
		if req.System[si].CacheControl != nil {
			req.System[si].CacheControl = nil
			dropped = true
		}
	}
	for mi, msg := range req.Messages {
		for pi, part := range msg.Parts {
			switch p := part.(type) {
			case ir.TextPart:
				if p.CacheControl != nil {
					p.CacheControl = nil
					msg.Parts[pi] = p
					dropped = true
				}
			case ir.ToolResultPart:
				if p.CacheControl != nil {
					p.CacheControl = nil
					msg.Parts[pi] = p
					dropped = true
				}
			}
		}
		req.Messages[mi] = msg
	}
	if dropped {
		trace.CompatPolicy("cache_control", CacheControlDrop, "cache_control markers dropped for non-Anthropic target")
	}
	return req
}

// emulatedToolSchema returns the client-tool input schema an emulated
// server tool presents to the model: the documented input object of the
// tool family (query for web_search, url for web_fetch), derived
// from the spec, never from a live capture of provider behavior.
var emulatedToolSchema = map[string]string{
	"web_search": `{"type":"object","properties":{"query":{"type":"string","description":"Search query"}},"required":["query"]}`,
	"web_fetch":  `{"type":"object","properties":{"url":{"type":"string","description":"URL to fetch (must appear in prior conversation context)"}},"required":["url"]}`,
}

// applyEmulation enforces the server_tool_emulation policy.
//
//   - native (default): server-tool defs ride untouched — the routed
//     provider has native support.
//   - emulate-firecrawl: each server-tool def becomes an emulated CLIENT
//     tool (Emulated marker, family name, family input schema, knobs
//     preserved for executor enforcement); the dated type string and any
//     missing ServerTool config are filled so the executor can enforce
//     max_uses / domain filters / citations / max_content_tokens.
//   - off: server-tool defs are removed — a provider that cannot serve
//     them must not receive them.
//
// History server-tool result blocks are never touched: they are past
// output being replayed, not requests for new capability.
func applyEmulation(c Compat, req ir.Request, trace *Trace) ir.Request {
	if c.ServerToolEmulation == "" || c.ServerToolEmulation == EmulationNative {
		return req
	}

	if !hasServerToolDef(req.Tools) {
		return req
	}

	kept := make([]ir.Tool, 0, len(req.Tools))
	removed, converted := 0, 0
	for _, tool := range req.Tools {
		family := tool.ServerToolName()
		if family == "" {
			kept = append(kept, tool)
			continue
		}
		if c.ServerToolEmulation == EmulationOff {
			removed++
			continue
		}
		// emulate-firecrawl: convert in place.
		st := tool.ServerTool
		if st == nil {
			st = &ir.ServerToolConfig{}
		}
		convertedTool := ir.Tool{
			Name:        family,
			Description: emulatedToolDescription(family, tool.Description),
			InputSchema: json.RawMessage(emulatedToolSchema[family]),
			ServerTool:  st, // knobs preserved for executor enforcement
			Emulated:    true,
		}
		kept = append(kept, convertedTool)
		converted++
	}

	if removed == 0 && converted == 0 {
		return req // hasServerToolDef guarded, but be explicit anyway
	}

	req.Tools = kept
	if c.ServerToolEmulation == EmulationOff {
		trace.CompatPolicy("server_tool_emulation", EmulationOff,
			strconv.Itoa(removed)+" server-tool def(s) removed (emulation off)")
	} else {
		trace.CompatPolicy("server_tool_emulation", EmulationEmulateFirecrawl,
			"server-tool def(s) converted to emulated client tool(s) (Firecrawl fallback)")
	}
	return req
}

// hasServerToolDef reports whether tools contain any server-tool family.
func hasServerToolDef(tools []ir.Tool) bool {
	for _, tool := range tools {
		if tool.ServerToolName() != "" {
			return true
		}
	}
	return false
}

// emulatedToolDescription builds the tool description the model sees: the
// family's documented purpose, preferring an explicit description the
// client supplied.
func emulatedToolDescription(family, clientDesc string) string {
	if clientDesc != "" {
		return clientDesc
	}
	switch family {
	case "web_search":
		return "Search the web with a search engine and return results."
	case "web_fetch":
		return "Fetch the content of a web page that appeared in the conversation."
	default:
		return "Server tool emulated by the proxy (" + family + ")."
	}
}
