// Package ir defines the canonical internal representation (IR) that all
// protocol codecs translate to and from. Hub-and-spoke: every protocol
// contributes a codec pair instead of N·(N−1) pairwise converters, so adding
// a provider adds one codec pair, never changes the others.
//
// Design rules (binding):
//   - Lossless-ish with explicit degradation: wire fields with no IR
//     equivalent ride in per-level extras maps and are re-emitted on
//     same-protocol round trips; crossing protocols drops them with a
//     debug log naming the exact field path. Documented per field in
//     docs/reference/translation-matrix.md (written with the codecs).
//   - Thinking blocks carry opaque signatures verbatim; codecs never
//     fabricate signature material.
//   - Cache markers are structural IR state, not passthrough.
package ir

import (
	"encoding/json"
	"strings"
)

// Role is a message role, normalized across protocols.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool" // OpenAI tool result messages
)

// StopReason normalizes the terminal reason across protocols (Anthropic:
// end_turn/tool_use/max_tokens/stop_sequence/pause_turn/refusal/
// model_context_window_exceeded; OpenAI: stop/length/tool_calls/
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

// CacheControl is the Anthropic prompt-caching marker — structural IR state
// so Anthropic→Anthropic requests forward it verbatim and other directions
// drop it with a logged degradation.
type CacheControl struct {
	Type string // "ephemeral"
	TTL  string // "5m" | "1h"; "" = protocol default
}

// Part is a typed content part. Variants are closed: text, image, tool call,
// tool result, thinking.
type Part interface{ part() }

// TextPart is textual content (or a system prompt block).
type TextPart struct {
	Text         string
	CacheControl *CacheControl
}

// ImagePart unifies Anthropic image blocks (base64 or URL source) and
// OpenAI image_url parts (URL or data: URI). The two wire forms are
// semantically interchangeable; codecs normalize on decode and reconstruct
// the protocol-appropriate form on encode.
type ImagePart struct {
	// Source is "base64", "url" or "file". Data: URIs from OpenAI decode to
	// base64 parts with MediaType set.
	Source    string
	MediaType string // image/png, image/jpeg, image/gif, image/webp
	Data      string // base64 payload when Source == "base64"
	URL       string // http(s) URL when Source == "url"
	// FileID is the Files-API upload reference when Source == "file". It is
	// opaque and account-scoped: the id rides for same-protocol round trips
	// (an origin account resolves it); a crossing protocol drops the image,
	// because no other wire can name the file.
	FileID string
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
// (the Anthropic block form) or OpenAI reasoning items with
// encrypted_content.
// Signatures ride verbatim — never fabricated, never recomputed.
type ThinkingPart struct {
	Thinking  string
	Signature string
}

func (TextPart) part()             {}
func (ImagePart) part()            {}
func (ToolCallPart) part()         {}
func (ToolResultPart) part()       {}
func (ThinkingPart) part()         {}
func (ServerToolUsePart) part()    {}
func (ServerToolResultPart) part() {}

// ServerToolUsePart is an Anthropic server-tool invocation: unlike
// ToolCallPart it executes on provider infrastructure (or, when emulated, on
// the proxy's web-tool backend) and needs no client tool_result.
// Input is the raw tool input object; Name is the stable family name
// ("web_search" / "web_fetch").
type ServerToolUsePart struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// WebSearchResultItem is one entry of a web_search_tool_result content
// array (verified live 2026-09-12: content is an ARRAY of these blocks,
// unlike web_fetch's single object).
type WebSearchResultItem struct {
	URL              string
	Title            string
	EncryptedContent string // provider-encrypted page content, opaque to the proxy
	PageAge          string // "" when unknown
}

// WebFetchContent is the normalized page payload of a web_fetch_tool_result
// (single web_fetch_result object). Source mirrors the Anthropic document
// block: text sources carry MediaType "text/plain" with the text in Data;
// PDF sources arrive as base64 Data with MediaType "application/pdf".
type WebFetchContent struct {
	URL              string
	Title            string
	Source           string // "text" | "base64"
	MediaType        string // "text/plain" | "application/pdf"
	Data             string // text payload (Source "text") or base64 payload ("base64")
	CitationsEnabled bool
	RetrievedAt      string // RFC 3339; "" when unknown
}

// ServerToolResultPart is the result of a server-tool invocation, paired
// with a ServerToolUsePart by ToolUseID. IsError distinguishes the wire's
// web_search_tool_result_error / web_fetch_tool_result_error shapes;
// ErrorCode carries the provider error code (invalid_tool_input,
// unavailable, max_uses_exceeded, too_many_requests, query_too_long,
// url_too_long, url_not_allowed, url_not_in_prior_context,
// url_not_accessible, unsupported_content_type).
type ServerToolResultPart struct {
	ToolUseID string
	ToolName  string // "web_search" | "web_fetch"
	IsError   bool
	ErrorCode string // set when IsError

	// Web_search carries an array of results; web_fetch a single page
	// object — the wire asymmetry is normalized here.
	Search []WebSearchResultItem
	Fetch  *WebFetchContent
}

// Message is one conversation turn. Tool results from Anthropic user
// messages decode into ToolResultPart parts; OpenAI role:"tool" messages
// decode into single-part messages whose part is a ToolResultPart.
type Message struct {
	Role  Role
	Parts []Part
}

// UserLocation is the web_search approximate-geolocation hint: type
// "approximate" plus city/region/country/timezone.
type UserLocation struct {
	Type     string // "approximate"
	City     string
	Region   string
	Country  string
	Timezone string
}

// ServerToolConfig carries the server-tool definition knobs the web-tool
// emulation layer enforces (max_uses, domain filters, user_location,
// citations, max_content_tokens). All optional; zero values = provider
// defaults.
type ServerToolConfig struct {
	MaxUses          int
	AllowedDomains   []string
	BlockedDomains   []string
	UserLocation     *UserLocation
	CitationsEnabled bool
	MaxContentTokens int
}

// Tool is a tool definition. Client tools are function-shaped; server
// tools carry a dated type string in the family web_search_20250305 /
// web_fetch_20250910 — the family name, not the date suffix, is the stable
// identity.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage

	// Type is the server-tool type string ("" for regular client tools).
	// It rides verbatim for Anthropic→Anthropic round trips: the wire type
	// is an open list, so an unrecognized value has to survive re-encode.
	// The compat policy maps a known family to a capability or an emulated
	// client tool.
	Type string

	// ServerTool carries the server-tool definition knobs when Type is a
	// server-tool family; nil for client tools.
	ServerTool *ServerToolConfig

	// Emulated marks a tool definition the emulation layer injected
	// (or converted from a native server-tool def under the
	// emulate-firecrawl compat policy). The executor's tool-call
	// round-trip loop recognizes model tool calls against it and answers
	// them via the configured web-tool accounts instead of surfacing them to the
	// client. False on everything decoded from a wire request — an
	// emulator-injected tool never rides back out to a client.
	Emulated bool
}

// serverToolFamilyPrefixes maps wire type-string prefixes to the stable
// server-tool family name: dated suffixes rotate, families don't.
var serverToolFamilyPrefixes = []string{"web_search_", "web_fetch_"}

// ServerToolName returns the stable server-tool family name for a
// server-tool type string ("web_search" / "web_fetch"), or "" when the
// tool is a regular client tool or an unrecognized server tool.
func (t Tool) ServerToolName() string {
	// The OpenAI Responses wire uses the undated family names
	// ("web_search", "web_fetch") while Anthropic uses dated type strings.
	switch t.Type {
	case "web_search":
		return "web_search"
	case "web_fetch":
		return "web_fetch"
	}
	for _, prefix := range serverToolFamilyPrefixes {
		if strings.HasPrefix(t.Type, prefix) {
			return strings.TrimSuffix(prefix, "_")
		}
	}
	return ""
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

// ThinkingConfig is the extended-thinking request parameter:
// budget_tokens >= 1024 when Enabled, or adaptive mode.
type ThinkingConfig struct {
	Enabled      bool
	BudgetTokens int
	Adaptive     bool
}

// EffortFromBudget maps a thinking budget_tokens to a reasoning-effort level.
// Tiers follow the shared codec heuristic (high >= 8192, medium >= 4096,
// low > 0); a zero budget has no effort mapping ("" — callers keep their
// protocol default).
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

	// System holds block-form system prompt parts (Anthropic system arrays
	// forward exactly; a plain string decodes to one TextPart).
	// OpenAI instructions decode into System[0].
	System []TextPart

	Messages   []Message
	Tools      []Tool
	ToolChoice *ToolChoice
	Thinking   *ThinkingConfig
	Stream     bool

	// ReasoningEffort is the explicit reasoning-effort hint (the compat
	// policy maps a thinking budget here; "" = unset). Provider codecs
	// prefer it over deriving effort from Thinking.BudgetTokens themselves —
	// the policy layer owns the decision so it is observable in route_trace,
	// never silent.
	ReasoningEffort string

	// ThinkingRequested records the compat layer's verdict on the CLIENT's
	// request: whether it enabled extended thinking (enabled or
	// adaptive) BEFORE the policy rewrote the parameter — a stripped or
	// mapped parameter must not erase the client's intent. No codec emits it
	// on the wire; the RESPONSE path reads it to decide whether the target's
	// own reasoning may surface as a thinking block (routing
	// applyResponseThinking). The reference returns thinking blocks only for
	// requests that enabled thinking, and a client that asked for none reads
	// the first content block as text — Claude Code's web_fetch helper
	// answers "No response from model" when it is a thinking block instead.
	ThinkingRequested bool

	// Cross-protocol request controls. These are explicit (not Passthrough)
	// because at least two wire families understand them; each encoder maps
	// what its protocol supports and leaves the rest to the trace.
	Metadata           map[string]string `json:"-"`
	Store              *bool             `json:"-"`
	PreviousResponseID string            `json:"-"`
	ServiceTier        string            `json:"-"`
	Include            []string          `json:"-"`
	ResponseFormat     json.RawMessage   `json:"-"` // chat response_format / responses text.format
	TextVerbosity      string            `json:"-"`
	ReasoningSummary   json.RawMessage   `json:"-"`
	ParallelToolCalls  *bool             `json:"-"`
	ClientMetadata     map[string]string `json:"-"`
	PromptCacheKey     string            `json:"-"`
	StreamOptions      json.RawMessage   `json:"-"`
	KeepAlive          json.RawMessage   `json:"-"`
	TopK               *int              `json:"-"`
	Seed               *int              `json:"-"`
	N                  *int              `json:"-"`
	User               string            `json:"-"`

	// OllamaOptions carries the raw /api/chat options object. Known keys are
	// decoded into the typed fields above; unknown keys are merged back on a
	// same-protocol re-encode so Ollama-specific knobs (num_ctx,
	// repeat_penalty, mirostat, ...) survive.
	OllamaOptions map[string]json.RawMessage `json:"-"`

	// SourceProtocol names the codec family that decoded this request
	// ("openai", "openai-responses", "anthropic", "ollama"). A same-protocol
	// encoder re-emits Passthrough; a crossing encoder drops it.
	SourceProtocol string

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
// map the Anthropic cache_creation_input_tokens / cache_read_input_tokens
// counters and Ollama prompt_eval_cached_count; absent elsewhere →
// explicit zeros.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	ReasoningTokens          int `json:"reasoning_tokens,omitempty"`

	// Server-tool request counters (server_tool_use.web_search_requests
	// / web_fetch_requests in the Anthropic usage object). Explicit zeros
	// like the cache fields; emulated
	// invocations count against these too so clients see honest accounting.
	WebSearchRequests int `json:"web_search_requests,omitempty"`
	WebFetchRequests  int `json:"web_fetch_requests,omitempty"`

	// Estimated marks heuristic counts (count_tokens fallback).
	Estimated bool `json:"-"`
}

// UpstreamError is a normalized upstream/API error.
type UpstreamError struct {
	HTTPStatus int    `json:"-"` // 0 when the error came from a stream event
	Type       string `json:"type,omitempty"`
	Code       string `json:"code,omitempty"`
	Message    string `json:"message,omitempty"`
}
