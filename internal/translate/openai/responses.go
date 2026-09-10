// Responses-API codec pair against the canonical IR. Wire shapes derive
// from the golden fixtures in testdata/golden/openai/responses-*
// (developers.openai.com/api/reference/resources/responses/methods/create,
// accessed 2026-09-10, facts O5–O8) and the streaming reference
// (developers.openai.com/api/docs/api-reference/responses-streaming,
// accessed 2026-09-10).
//
// Key wire differences from chat-completions (O5, O6):
//   - `input` is a plain string or a typed item array (not `messages`).
//   - `instructions` carries the system prompt (plus system-role items).
//   - function tools are FLAT: {type:"function", name, ...} with no nested
//     function wrapper, and tool_choice objects are {type:"function", name}.
//   - tool calls are first-class `function_call` output items keyed by
//     `call_id`; results come back as `function_call_output` items.
//   - usage is {input_tokens, output_tokens, output_tokens_details:
//     {reasoning_tokens}, total_tokens}.
package openai

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/proxyllm/proxyllm/internal/translate"
	"github.com/proxyllm/proxyllm/internal/translate/ir"
)

// ResponsesCodec decodes and encodes OpenAI Responses-API wire bodies.
type ResponsesCodec struct{}

// DecodeRequest decodes a Responses-API request body into the IR.
func (ResponsesCodec) DecodeRequest(raw []byte) (ir.Request, error) {
	var w responsesWireRequest
	if err := json.Unmarshal(raw, &w); err != nil {
		return ir.Request{}, fmt.Errorf("openai/responses: decode request: %w", err)
	}
	req := ir.Request{
		Model:       w.Model,
		MaxTokens:   w.MaxOutputTokens,
		Temperature: w.Temperature,
		TopP:        w.TopP,
		Stream:      w.Stream,
		ToolChoice:  responsesToolChoiceFromWire(w.ToolChoice),
	}
	if w.Instructions != "" {
		// instructions is the Responses system prompt (O5).
		req.System = append(req.System, ir.TextPart{Text: w.Instructions})
	}
	if w.Input != nil {
		for _, item := range w.Input.Items {
			req.Messages = append(req.Messages, responsesMessageFromItem(item)...)
		}
		if w.Input.Str != nil {
			req.Messages = append(req.Messages, ir.Message{
				Role:  ir.RoleUser,
				Parts: []ir.Part{ir.TextPart{Text: *w.Input.Str}},
			})
		}
	}
	for _, wt := range w.Tools {
		if wt.Name != "" {
			req.Tools = append(req.Tools, ir.Tool{
				Name:        wt.Name,
				Description: wt.Description,
				InputSchema: wt.Parameters,
			})
		}
	}
	if w.Reasoning != nil && w.Reasoning.Effort != "" {
		// reasoning.effort marks enabled thinking; the budget is not on the
		// wire, so BudgetTokens stays 0 (O5) — the effort level rides in
		// Passthrough-free form and is reconstructed on encode.
		req.Thinking = &ir.ThinkingConfig{Enabled: true}
	}
	return req, nil
}

// EncodeRequest encodes an IR request into a Responses-API body.
func (ResponsesCodec) EncodeRequest(req ir.Request) ([]byte, error) {
	w := responsesWireRequest{
		Model:           req.Model,
		MaxOutputTokens: req.MaxTokens,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		Stream:          req.Stream,
		ToolChoice:      responsesToolChoiceToWire(req.ToolChoice),
	}
	if len(req.System) > 0 {
		texts := make([]string, 0, len(req.System))
		for _, sys := range req.System {
			texts = append(texts, sys.Text)
		}
		w.Instructions = strings.Join(texts, "\n\n")
	}
	input := &responsesWireInput{}
	for _, m := range req.Messages {
		input.Items = append(input.Items, responsesItemsFromMessage(m)...)
	}
	w.Input = input
	for _, t := range req.Tools {
		w.Tools = append(w.Tools, responsesWireTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		})
	}
	if req.Thinking != nil && req.Thinking.Enabled {
		w.Reasoning = &responsesWireReasoning{Effort: effortFor(req)}
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("openai/responses: encode request: %w", err)
	}
	return raw, nil
}

// DecodeResponse decodes a Responses-API response body into the IR.
func (ResponsesCodec) DecodeResponse(raw []byte) (ir.Response, error) {
	var w responsesWireResponse
	if err := json.Unmarshal(raw, &w); err != nil {
		return ir.Response{}, fmt.Errorf("openai/responses: decode response: %w", err)
	}
	resp := ir.Response{
		ID:    w.ID,
		Model: w.Model,
		Usage: responsesUsageFromWire(w.Usage),
	}
	hasToolCall := false
	for _, item := range w.Output {
		switch item.Type {
		case "message":
			resp.Content = append(resp.Content, responsesPartsFromContent(item.Content)...)
		case "function_call":
			hasToolCall = true
			resp.Content = append(resp.Content, ir.ToolCallPart{
				ID:    item.CallID,
				Name:  item.Name,
				Input: json.RawMessage(item.Arguments),
			})
		case "reasoning":
			// O8: reasoning items carry summary text + encrypted_content;
			// the encrypted content rides verbatim as the signature (never
			// fabricated, A12).
			resp.Content = append(resp.Content, ir.ThinkingPart{
				Thinking:  responsesSummaryText(item.Summary),
				Signature: item.EncryptedContent,
			})
		}
	}
	switch w.Status {
	case "incomplete":
		resp.StopReason = ir.StopMaxTokens
	default:
		if hasToolCall {
			resp.StopReason = ir.StopToolUse
		} else {
			resp.StopReason = ir.StopEndTurn
		}
	}
	return resp, nil
}

// EncodeResponse encodes an IR response into a Responses-API body.
func (ResponsesCodec) EncodeResponse(resp ir.Response) ([]byte, error) {
	w := responsesWireResponse{
		ID:     resp.ID,
		Object: "response",
		Status: "completed",
		Model:  resp.Model,
		Usage:  responsesUsageToWire(resp.Usage),
	}
	if resp.StopReason == ir.StopMaxTokens {
		w.Status = "incomplete"
	}
	for _, p := range resp.Content {
		switch pt := p.(type) {
		case ir.TextPart:
			w.Output = append(w.Output, responsesWireItem{
				Type:    "message",
				Status:  "completed",
				Role:    "assistant",
				Content: responsesContentFromParts([]ir.Part{pt}, "output_text"),
			})
		case ir.ToolCallPart:
			args := string(pt.Input)
			if args == "" {
				args = "{}"
			}
			w.Output = append(w.Output, responsesWireItem{
				Type:      "function_call",
				CallID:    pt.ID,
				Name:      pt.Name,
				Arguments: args,
			})
		case ir.ThinkingPart:
			item := responsesWireItem{Type: "reasoning"}
			if pt.Thinking != "" {
				item.Summary = []responsesWirePart{{Type: "summary_text", Text: pt.Thinking}}
			}
			item.EncryptedContent = pt.Signature
			w.Output = append(w.Output, item)
		}
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("openai/responses: encode response: %w", err)
	}
	return raw, nil
}

// NewResponsesStreamDecoder returns the Responses-API SSE stream decoder for r.
func NewResponsesStreamDecoder(r io.Reader) translate.StreamDecoder {
	return newResponsesStreamDecoder(r)
}

// ---- wire types (mirror the golden fixtures) ----

// responsesWireInput is the request `input`: a plain string or an item array
// (O5).
type responsesWireInput struct {
	Str   *string
	Items []responsesWireItem
}

func (in *responsesWireInput) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		in.Str = &s
		return nil
	}
	return json.Unmarshal(b, &in.Items)
}

func (in responsesWireInput) MarshalJSON() ([]byte, error) {
	if in.Str != nil {
		return json.Marshal(*in.Str)
	}
	return json.Marshal(in.Items)
}

// responsesWireContent is a message item's `content`: a plain string or a
// part array (input_text/output_text/input_image, O5, O11).
type responsesWireContent struct {
	Str   *string
	Parts []responsesWirePart
}

func (c *responsesWireContent) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		c.Str = &s
		return nil
	}
	return json.Unmarshal(b, &c.Parts)
}

func (c responsesWireContent) MarshalJSON() ([]byte, error) {
	if c.Str != nil {
		return json.Marshal(*c.Str)
	}
	if c.Parts == nil {
		return []byte("null"), nil
	}
	return json.Marshal(c.Parts)
}

type responsesWirePart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// responsesWireItem is one input or output item: message, function_call,
// function_call_output, or reasoning (O5, O8).
type responsesWireItem struct {
	Type   string `json:"type"`
	ID     string `json:"id,omitempty"`
	Status string `json:"status,omitempty"`
	Role   string `json:"role,omitempty"`

	Content *responsesWireContent `json:"content,omitempty"`

	// function_call / function_call_output.
	CallID    string                `json:"call_id,omitempty"`
	Name      string                `json:"name,omitempty"`
	Arguments string                `json:"arguments,omitempty"`
	Output    *responsesWireContent `json:"output,omitempty"`

	// reasoning (O8).
	Summary          []responsesWirePart `json:"summary,omitempty"`
	EncryptedContent string              `json:"encrypted_content,omitempty"`
}

// responsesWireTool is a FLAT function tool: no nested function wrapper
// (O6).
type responsesWireTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// responsesToolChoice is the tool_choice value: a string or a flat
// {type:"function", name} object (O5).
type responsesToolChoice struct {
	Str    *string
	Object *responsesToolChoiceObject
}

type responsesToolChoiceObject struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

func (c *responsesToolChoice) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		c.Str = &s
		return nil
	}
	return json.Unmarshal(b, &c.Object)
}

func (c responsesToolChoice) MarshalJSON() ([]byte, error) {
	if c.Str != nil {
		return json.Marshal(*c.Str)
	}
	return json.Marshal(c.Object)
}

type responsesWireReasoning struct {
	Effort  string          `json:"effort,omitempty"`
	Summary json.RawMessage `json:"summary,omitempty"`
}

type responsesWireRequest struct {
	Model           string                  `json:"model"`
	Input           *responsesWireInput     `json:"input"`
	Instructions    string                  `json:"instructions,omitempty"`
	Tools           []responsesWireTool     `json:"tools,omitempty"`
	ToolChoice      *responsesToolChoice    `json:"tool_choice,omitempty"`
	MaxOutputTokens int                     `json:"max_output_tokens,omitempty"`
	Temperature     *float64                `json:"temperature,omitempty"`
	TopP            *float64                `json:"top_p,omitempty"`
	Reasoning       *responsesWireReasoning `json:"reasoning,omitempty"`
	Stream          bool                    `json:"stream,omitempty"`
}

type responsesWireUsageDetails struct {
	CachedTokens    int `json:"cached_tokens"`
	ReasoningTokens int `json:"reasoning_tokens"`
}

type responsesWireUsage struct {
	InputTokens         int                        `json:"input_tokens"`
	OutputTokens        int                        `json:"output_tokens"`
	InputTokensDetails  *responsesWireUsageDetails `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *responsesWireUsageDetails `json:"output_tokens_details,omitempty"`
	TotalTokens         int                        `json:"total_tokens"`
}

type responsesWireResponse struct {
	ID        string              `json:"id"`
	Object    string              `json:"object,omitempty"`
	CreatedAt int64               `json:"created_at,omitempty"`
	Status    string              `json:"status"`
	Error     *wireAPIError       `json:"error,omitempty"`
	Model     string              `json:"model"`
	Output    []responsesWireItem `json:"output"`
	Usage     *responsesWireUsage `json:"usage,omitempty"`
}

// ---- conversions ----

// responsesMessageFromItem maps one input item onto IR messages.
func responsesMessageFromItem(item responsesWireItem) []ir.Message {
	switch item.Type {
	case "message":
		switch item.Role {
		case "system", "developer":
			if item.Content != nil {
				if text := responsesContentText(item.Content); text != "" {
					return []ir.Message{{Role: ir.RoleSystem, Parts: []ir.Part{ir.TextPart{Text: text}}}}
				}
			}
			return nil
		case "assistant":
			return []ir.Message{{
				Role:  ir.RoleAssistant,
				Parts: responsesPartsFromContent(item.Content),
			}}
		default: // user
			return []ir.Message{{
				Role:  ir.RoleUser,
				Parts: responsesPartsFromContent(item.Content),
			}}
		}
	case "function_call":
		// call_id keys the call; arguments is a JSON string (O5).
		return []ir.Message{{
			Role: ir.RoleAssistant,
			Parts: []ir.Part{ir.ToolCallPart{
				ID:    item.CallID,
				Name:  item.Name,
				Input: json.RawMessage(item.Arguments),
			}},
		}}
	case "function_call_output":
		content := responsesPartsFromContent(item.Output)
		if content == nil {
			content = []ir.Part{ir.TextPart{}}
		}
		return []ir.Message{{
			Role: ir.RoleTool,
			Parts: []ir.Part{ir.ToolResultPart{
				ToolUseID: item.CallID,
				Content:   content,
			}},
		}}
	case "reasoning":
		return []ir.Message{{
			Role: ir.RoleAssistant,
			Parts: []ir.Part{ir.ThinkingPart{
				Thinking:  responsesSummaryText(item.Summary),
				Signature: item.EncryptedContent,
			}},
		}}
	default:
		return nil
	}
}

// responsesItemsFromMessage maps one IR message onto input items.
func responsesItemsFromMessage(m ir.Message) []responsesWireItem {
	switch m.Role {
	case ir.RoleSystem:
		// System prompt rides in `instructions` (encode side); system
		// messages have no Responses input equivalent beyond system-role
		// items, which decode consumes above.
		var text string
		for _, p := range m.Parts {
			if tp, ok := p.(ir.TextPart); ok {
				if text != "" {
					text += "\n\n"
				}
				text += tp.Text
			}
		}
		return []responsesWireItem{{Type: "message", Role: "system", Content: &responsesWireContent{Str: &text}}}
	case ir.RoleUser:
		return []responsesWireItem{{
			Type:    "message",
			Role:    "user",
			Content: responsesContentFromParts(m.Parts, "input_text"),
		}}
	case ir.RoleAssistant:
		var items []responsesWireItem
		var textParts []ir.Part
		for _, p := range m.Parts {
			switch pt := p.(type) {
			case ir.ToolCallPart:
				args := string(pt.Input)
				if args == "" {
					args = "{}"
				}
				items = append(items, responsesWireItem{
					Type:      "function_call",
					CallID:    pt.ID,
					Name:      pt.Name,
					Arguments: args,
				})
			case ir.ThinkingPart:
				item := responsesWireItem{Type: "reasoning"}
				if pt.Thinking != "" {
					item.Summary = []responsesWirePart{{Type: "summary_text", Text: pt.Thinking}}
				}
				item.EncryptedContent = pt.Signature
				items = append(items, item)
			default:
				textParts = append(textParts, pt)
			}
		}
		if len(textParts) > 0 {
			items = append(items, responsesWireItem{
				Type:    "message",
				Status:  "completed",
				Role:    "assistant",
				Content: responsesContentFromParts(textParts, "output_text"),
			})
		}
		return items
	case ir.RoleTool:
		var items []responsesWireItem
		for _, p := range m.Parts {
			if tr, ok := p.(ir.ToolResultPart); ok {
				items = append(items, responsesWireItem{
					Type:   "function_call_output",
					CallID: tr.ToolUseID,
					Output: responsesContentFromParts(tr.Content, "output_text"),
				})
			}
		}
		return items
	default:
		return nil
	}
}

func responsesPartsFromContent(c *responsesWireContent) []ir.Part {
	if c == nil {
		return nil
	}
	var parts []ir.Part
	if c.Str != nil && *c.Str != "" {
		parts = append(parts, ir.TextPart{Text: *c.Str})
	}
	for _, p := range c.Parts {
		switch p.Type {
		case "input_text", "output_text", "summary_text", "refusal":
			parts = append(parts, ir.TextPart{Text: p.Text})
		case "input_image":
			img := ir.ImagePart{Source: "url", URL: p.ImageURL, Detail: p.Detail}
			if len(p.ImageURL) > 5 && p.ImageURL[:5] == "data:" {
				// data: URIs carry the base64 payload inline (O11).
				img.Source = "base64"
			}
			parts = append(parts, img)
		}
	}
	return parts
}

func responsesContentFromParts(parts []ir.Part, textType string) *responsesWireContent {
	if len(parts) == 0 {
		return nil
	}
	if len(parts) == 1 {
		if tp, ok := parts[0].(ir.TextPart); ok {
			s := tp.Text
			return &responsesWireContent{Str: &s}
		}
	}
	c := &responsesWireContent{}
	for _, p := range parts {
		switch pt := p.(type) {
		case ir.TextPart:
			c.Parts = append(c.Parts, responsesWirePart{Type: textType, Text: pt.Text})
		case ir.ImagePart:
			c.Parts = append(c.Parts, responsesWirePart{
				Type:     "input_image",
				ImageURL: imageURL(pt),
				Detail:   pt.Detail,
			})
		}
	}
	if c.Parts == nil {
		return nil
	}
	return c
}

func responsesContentText(c *responsesWireContent) string {
	if c.Str != nil {
		return *c.Str
	}
	var out []string
	for _, p := range c.Parts {
		if p.Type == "input_text" || p.Type == "output_text" {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "\n\n")
}

func responsesSummaryText(summary []responsesWirePart) string {
	var out []string
	for _, p := range summary {
		if p.Type == "summary_text" {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "\n")
}

func responsesToolChoiceFromWire(w *responsesToolChoice) *ir.ToolChoice {
	if w == nil {
		return nil
	}
	if w.Str != nil {
		switch *w.Str {
		case "auto":
			return &ir.ToolChoice{Type: ir.ToolChoiceAuto}
		case "none":
			return &ir.ToolChoice{Type: ir.ToolChoiceNone}
		case "required":
			return &ir.ToolChoice{Type: ir.ToolChoiceAny}
		}
		return nil
	}
	if w.Object != nil && w.Object.Type == "function" {
		return &ir.ToolChoice{Type: ir.ToolChoiceTool, Name: w.Object.Name}
	}
	return nil
}

func responsesToolChoiceToWire(tc *ir.ToolChoice) *responsesToolChoice {
	if tc == nil {
		return nil
	}
	switch tc.Type {
	case ir.ToolChoiceAuto, ir.ToolChoiceNone, ir.ToolChoiceAny:
		s := map[ir.ToolChoiceType]string{
			ir.ToolChoiceAuto: "auto",
			ir.ToolChoiceNone: "none",
			ir.ToolChoiceAny:  "required",
		}[tc.Type]
		return &responsesToolChoice{Str: &s}
	case ir.ToolChoiceTool:
		return &responsesToolChoice{Object: &responsesToolChoiceObject{
			Type: "function",
			Name: tc.Name,
		}}
	default:
		return nil
	}
}

func responsesUsageFromWire(w *responsesWireUsage) ir.Usage {
	if w == nil {
		return ir.Usage{}
	}
	u := ir.Usage{
		InputTokens:  w.InputTokens,
		OutputTokens: w.OutputTokens,
	}
	if w.InputTokensDetails != nil {
		u.CacheReadInputTokens = w.InputTokensDetails.CachedTokens
	}
	if w.OutputTokensDetails != nil {
		u.ReasoningTokens = w.OutputTokensDetails.ReasoningTokens
	}
	return u
}

func responsesUsageToWire(u ir.Usage) *responsesWireUsage {
	w := &responsesWireUsage{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		TotalTokens:  u.InputTokens + u.OutputTokens,
	}
	if u.CacheReadInputTokens != 0 {
		w.InputTokensDetails = &responsesWireUsageDetails{CachedTokens: u.CacheReadInputTokens}
	}
	if u.ReasoningTokens != 0 {
		w.OutputTokensDetails = &responsesWireUsageDetails{ReasoningTokens: u.ReasoningTokens}
	}
	return w
}

// effortFromBudget reconstructs a reasoning effort level from the IR budget
// (inverse of the budget-less decode; thresholds mirror the ollama codec's
// budget→effort mapping).
func effortFromBudget(budget int) string {
	switch {
	case budget >= 8192:
		return "high"
	case budget >= 4096:
		return "medium"
	case budget > 0:
		return "low"
	default:
		return "medium"
	}
}

// effortFor picks the reasoning effort for a request: the §6.4
// compat-policy mapping (ir.ReasoningEffort, set by routing) wins over the
// codec's own budget heuristic — the policy layer owns the decision so it
// is observable in route_trace, never silent.
func effortFor(req ir.Request) string {
	if req.ReasoningEffort != "" {
		return req.ReasoningEffort
	}
	return effortFromBudget(req.Thinking.BudgetTokens)
}
