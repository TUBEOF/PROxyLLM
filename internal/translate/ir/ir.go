// Package ir defines the canonical internal representation (IR) that all
// protocol codecs translate to and from. Plan Analysis D: hub-and-spoke —
// every protocol contributes a codec pair instead of N·(N−1) pairwise
// converters, so adding a provider adds one codec pair, never changes the
// others.
//
// Design rules (plan Analysis D, binding):
//   - Lossless-ish with explicit degradation: wire fields with no IR
//     equivalent ride in per-level extras maps and are re-emitted on
//     same-protocol round trips; crossing protocols drops them with a
//     debug log naming the exact field path. Documented per field in
//     docs/reference/translation-matrix.md (written with the codecs).
//   - Thinking blocks carry opaque signatures verbatim; codecs never
//     fabricate signature material (A12).
//   - Cache markers are structural IR state, not passthrough (C4).
package ir

import "encoding/json"

// Role is a message role, normalized across protocols.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool" // OpenAI tool result messages
)

// StopReason normalizes the terminal reason across protocols
// (Anthropic A4: end_turn/tool_use/max_tokens/stop_sequence/pause_turn/
// refusal/model_context_window_exceeded; OpenAI O12: stop/length/tool_calls/
// content_filter; Ollama done_reason).
type StopReason string

const (
	StopEndTurn       StopReason = "end_turn" // natural completion ("stop")
	StopToolUse       StopReason = "tool_use" // model requests tool calls
	StopMaxTokens     StopReason = "max_tokens"
	StopStopSequence  StopReason = "stop_sequence"
	StopContentFilter StopReason = "content_filter"
	StopRefusal       StopReason = "refusal"
	StopPauseTurn     StopReason = "pause_turn"
	StopContextWindow StopReason = "model_context_window_exceeded"
)

// CacheControl is the Anthropic prompt-caching marker (A11) — structural IR
// state so Anthropic→Anthropic requests forward it verbatim (C4) and other
// directions drop it with a logged degradation.
type CacheControl struct {
	Type string // "ephemeral"
	TTL  string // "5m" | "1h"; "" = protocol default
}

// Part is a typed content part. Variants are closed: text, image, tool call,
// tool result, thinking (Analysis D).
type Part interface{ part() }

// TextPart is textual content (or a system prompt block).
type TextPart struct {
	Text         string
	CacheControl *CacheControl
}

// ImagePart unifies Anthropic image blocks (base64 or URL source, A3) and
// OpenAI image_url parts (URL or data: URI, O11). The two wire forms are
// semantically interchangeable; codecs normalize on decode and reconstruct
// the protocol-appropriate form on encode.
type ImagePart struct {
	// Source is "base64" or "url". Data: URIs from OpenAI decode to
	// base64 parts with MediaType set.
	Source    string
	MediaType string // image/png, image/jpeg, image/gif, image/webp
	Data      string // base64 payload when Source == "base64"
	URL       string // http(s) URL when Source == "url"
	// Detail is the OpenAI fidelity hint (low/high/original/auto);
	// "" = protocol default.
	Detail string
}

// ToolCallPart is a model-initiated tool invocation. Input is a JSON object;
// during stream assembly it may be a partial JSON fragment.
type ToolCallPart struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResultPart is the client-side answer to a ToolCallPart. Content holds
// text/image parts of the tool output.
type ToolResultPart struct {
	ToolUseID    string
	Content      []Part
	IsError      bool
	CacheControl *CacheControl
}

// ThinkingPart is extended-thinking content with its opaque signature
// (Anthropic A12) or OpenAI reasoning items with encrypted_content (O8).
// Signatures ride verbatim — never fabricated, never recomputed.
type ThinkingPart struct {
	Thinking  string
	Signature string
}

func (TextPart) part()       {}
func (ImagePart) part()      {}
func (ToolCallPart) part()   {}
func (ToolResultPart) part() {}
func (ThinkingPart) part()   {}

// Message is one conversation turn. Tool results from Anthropic user
// messages decode into ToolResultPart parts; OpenAI role:"tool" messages
// decode into single-part messages whose part is a ToolResultPart.
type Message struct {
	Role  Role
	Parts []Part
}

// Tool is a client tool definition. InputSchema is a JSON Schema object.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// ToolChoiceType enumerates tool-selection policies.
type ToolChoiceType string

const (
	ToolChoiceAuto ToolChoiceType = "auto"
	ToolChoiceAny  ToolChoiceType = "any"
	ToolChoiceTool ToolChoiceType = "tool"
	ToolChoiceNone ToolChoiceType = "none"
)

// ToolChoice selects tool behavior for a request.
type ToolChoice struct {
	Type ToolChoiceType
	Name string // required when Type == ToolChoiceTool
}

// ThinkingConfig is the extended-thinking request parameter (A12):
// budget_tokens >= 1024 when Enabled, or adaptive mode.
type ThinkingConfig struct {
	Enabled      bool
	BudgetTokens int
	Adaptive     bool
}

// EffortFromBudget maps a thinking budget_tokens to a reasoning-effort level
// (§6.4 map-to-reasoning-effort). Tiers follow the shared codec heuristic
// (high >= 8192, medium >= 4096, low > 0); a zero budget has no effort
// mapping ("" — callers keep their protocol default).
func EffortFromBudget(budget int) string {
	switch {
	case budget >= 8192:
		return "high"
	case budget >= 4096:
		return "medium"
	case budget > 0:
		return "low"
	default:
		return ""
	}
}

// Request is the canonical request IR.
type Request struct {
	Model         string
	MaxTokens     int // 0 = provider default
	StopSequences []string
	Temperature   *float64
	TopP          *float64

	// System holds block-form system prompt parts (C4: Anthropic system
	// arrays forward exactly; a plain string decodes to one TextPart).
	// OpenAI instructions decode into System[0].
	System []TextPart

	Messages   []Message
	Tools      []Tool
	ToolChoice *ToolChoice
	Thinking   *ThinkingConfig
	Stream     bool

	// ReasoningEffort is the explicit reasoning-effort hint (§6.4
	// map-to-reasoning-effort: the compat policy maps a thinking budget
	// here; "" = unset). Provider codecs prefer it over deriving effort
	// from Thinking.BudgetTokens themselves — the policy layer owns the
	// decision so it is observable in route_trace, never silent.
	ReasoningEffort string

	// Passthrough carries wire fields with no IR equivalent, keyed by
	// top-level field name (nested unknowns live in the codec wire
	// structs' extras). Same-protocol encoders re-emit them; crossing
	// protocols drops them with a debug log naming the field path.
	Passthrough map[string]json.RawMessage
}

// Response is the canonical non-streaming response IR.
type Response struct {
	ID           string
	Model        string
	StopReason   StopReason
	StopSequence string
	Content      []Part
	Usage        Usage

	Passthrough map[string]json.RawMessage
}

// Usage is the normalized token accounting across protocols. Cache fields
// map Anthropic A11 / Ollama prompt_eval_cached_count; absent elsewhere →
// explicit zeros.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	ReasoningTokens          int `json:"reasoning_tokens,omitempty"`
	// Estimated marks heuristic counts (count_tokens fallback, C1).
	Estimated bool `json:"-"`
}

// UpstreamError is a normalized upstream/API error.
type UpstreamError struct {
	HTTPStatus int    `json:"-"` // 0 when the error came from a stream event (A13)
	Type       string `json:"type,omitempty"`
	Code       string `json:"code,omitempty"`
	Message    string `json:"message,omitempty"`
}
