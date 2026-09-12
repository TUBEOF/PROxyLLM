package routing

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/proxyllm/proxyllm/internal/translate/ir"
)

// Compat is the per-model compatibility policy (plan §6.4, OP-2 resolution;
// storage: models.compat JSON, WebUI "Models & Compatibility" editor). It
// governs how requests for a target model are adapted before dispatch —
// every application is recorded in the Trace, never silent.
type Compat struct {
	// ThinkingHandling governs the thinking parameter and thinking blocks
	// in history: strip / map-to-reasoning-effort / block ("" = strip,
	// the default for non-Claude targets).
	ThinkingHandling string

	// ServerToolEmulation governs the Firecrawl web-tool fallback (§6.2):
	// native / emulate-firecrawl / off ("" = native).
	ServerToolEmulation string

	// MaxTokensClamp caps MaxTokens for targets with lower output limits
	// (C7: 32000 for unknown model IDs). 0 = off.
	MaxTokensClamp int

	// ContextWindow is the model context window (tokens) for capability
	// filtering and the count_tokens heuristic. 0 = unknown.
	ContextWindow int

	// CacheControl governs cache_control markers: forward / drop (""
	// = forward). Forward applies on Anthropic→Anthropic only (C4); drop
	// removes the markers and logs the degradation.
	CacheControl string
}

// ThinkingHandling values (§6.4).
const (
	// ThinkingStrip removes the thinking parameter and thinking blocks
	// from history — never sends fabricated signatures upstream (A12).
	// Default for non-Claude targets.
	ThinkingStrip = "strip"

	// ThinkingMapToEffort converts the thinking budget to a
	// reasoning-effort hint on the request (ir.ReasoningEffort) where the
	// target supports it (Ollama L5/L9, OpenAI O5).
	ThinkingMapToEffort = "map-to-reasoning-effort"

	// ThinkingBlock refuses thinking requests with a protocol-conform
	// error (operators opting into strictness).
	ThinkingBlock = "block"
)

// ServerToolEmulation values (§6.2).
const (
	EmulationNative           = "native"
	EmulationEmulateFirecrawl = "emulate-firecrawl"
	EmulationOff              = "off"
)

// CacheControl values (§6.4, C4).
const (
	CacheControlForward = "forward"
	CacheControlDrop    = "drop"
)

// ErrThinkingBlocked is the terminal error for the block policy.
var ErrThinkingBlocked = errors.New("routing: thinking requests are blocked by the model's thinking_handling policy")

// ApplyCompat adapts req per the per-model compatibility policy, recording
// every application in a fresh Trace (§6.4: never silent — each event names
// the setting and its applied value).
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
// target (A12).
func applyThinking(c Compat, req ir.Request, trace *Trace) (ir.Request, error) {
	setting := c.ThinkingHandling
	if setting == "" {
		setting = ThinkingStrip // §6.4 default for unset policies
	}

	hasThinking := req.Thinking != nil && (req.Thinking.Enabled || req.Thinking.Adaptive)

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
		// History thinking blocks cannot ride a non-thinking target (A12):
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
// (never upstream: fabricated/replayed signatures are an A12 violation).
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
// applies Anthropic→Anthropic only — C4 — enforced by the caller choosing
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
// tool family (A8/A9 — query for web_search, url for web_fetch), derived
// from the spec, never from a live capture of provider behavior.
var emulatedToolSchema = map[string]string{
	"web_search": `{"type":"object","properties":{"query":{"type":"string","description":"Search query"}},"required":["query"]}`,
	"web_fetch":  `{"type":"object","properties":{"url":{"type":"string","description":"URL to fetch (must appear in prior conversation context)"}},"required":["url"]}`,
}

// applyEmulation enforces the server_tool_emulation policy (§6.2/§6.4).
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

// ThinkingBudget returns the request's thinking budget (0 when unset).
func ThinkingBudget(req ir.Request) int {
	if req.Thinking == nil {
		return 0
	}
	return req.Thinking.BudgetTokens
}
