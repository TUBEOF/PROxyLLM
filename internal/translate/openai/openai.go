// Package openai implements the OpenAI chat-completions codec pair against
// the canonical IR (plan Analysis D). Wire shapes derive from the golden
// fixtures in testdata/golden/openai/ (developers.openai.com, accessed
// 2026-09-10, facts O2–O4, O11, O12).
package openai

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/tubeof/proxyllm/internal/translate"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// Codec decodes and encodes OpenAI chat-completions wire bodies.
type Codec struct{}

// DecodeRequest decodes an OpenAI chat-completions request body into the IR.
func (Codec) DecodeRequest(raw []byte) (ir.Request, error) {
	var w wireRequest
	if err := json.Unmarshal(raw, &w); err != nil {
		return ir.Request{}, fmt.Errorf("openai: decode request: %w", err)
	}
	req := ir.Request{
		Model:       w.Model,
		MaxTokens:   w.MaxCompletionTokens,
		Temperature: w.Temperature,
		TopP:        w.TopP,
		Stream:      w.Stream,
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = w.MaxTokens // legacy field name
	}
	for _, wm := range w.Messages {
		switch wm.Role {
		case "system", "developer":
			// System/developer precedence collapses into the IR system
			// list in wire order (O2).
			req.System = append(req.System, ir.TextPart{Text: wm.contentText()})
		case "user":
			req.Messages = append(req.Messages, ir.Message{
				Role:  ir.RoleUser,
				Parts: partsFromContent(wm.Content),
			})
		case "assistant":
			msg := ir.Message{Role: ir.RoleAssistant}
			msg.Parts = partsFromContent(wm.Content)
			for _, wtc := range wm.ToolCalls {
				msg.Parts = append(msg.Parts, toolCallFromWire(wtc))
			}
			req.Messages = append(req.Messages, msg)
		case "tool":
			tr := ir.ToolResultPart{
				ToolUseID: wm.ToolCallID,
				Content:   partsFromContent(wm.Content),
			}
			req.Messages = append(req.Messages, ir.Message{Role: ir.RoleTool, Parts: []ir.Part{tr}})
		}
	}
	for _, wt := range w.Tools {
		if wt.Function.Name != "" {
			req.Tools = append(req.Tools, ir.Tool{
				Name:        wt.Function.Name,
				Description: wt.Function.Description,
				InputSchema: wt.Function.Parameters,
			})
		}
	}
	if w.ToolChoice != nil {
		req.ToolChoice = toolChoiceFromWire(w.ToolChoice)
	}
	return req, nil
}

// EncodeRequest encodes an IR request into an OpenAI chat-completions body.
func (Codec) EncodeRequest(req ir.Request) ([]byte, error) {
	w := wireRequest{
		Model:               req.Model,
		MaxCompletionTokens: req.MaxTokens,
		Temperature:         req.Temperature,
		TopP:                req.TopP,
		Stream:              req.Stream,
	}
	// Ask the upstream to close a streamed response with a usage chunk.
	// Without it an OpenAI-compatible upstream reports no token counts at
	// all on the stream, and the request log can only record zeros — the
	// log's token/TPS columns are useless. Only meaningful while streaming
	// (the field is otherwise ignored, and some upstreams reject it on a
	// non-streaming request), so it rides the Stream flag.
	if req.Stream {
		w.StreamOptions = &wireStreamOptions{IncludeUsage: true}
	}
	for _, sys := range req.System {
		w.Messages = append(w.Messages, wireMessage{Role: "system", Content: wireContent{Str: &sys.Text}})
	}
	for _, m := range req.Messages {
		wm := wireMessage{Role: string(m.Role)}
		switch m.Role {
		case ir.RoleSystem:
			// A system-role message inside messages (Claude Code appends
			// one for hook output / injected reminders; the Anthropic
			// codec decodes it verbatim). Falls through to the OpenAI
			// chat system message — and must never leave the zero-value
			// wireContent whose marshal is "content":null (Ollama 400
			// invalid message content type: <nil>).
			var sb strings.Builder
			for _, p := range m.Parts {
				if tp, ok := p.(ir.TextPart); ok {
					sb.WriteString(tp.Text)
				}
			}
			text := sb.String()
			wm.Content = wireContent{Str: &text}
		case ir.RoleUser:
			wm.Content = contentFromParts(m.Parts)
		case ir.RoleAssistant:
			wm.Content = contentFromParts(m.Parts)
			for _, p := range m.Parts {
				if tc, ok := p.(ir.ToolCallPart); ok {
					wm.ToolCalls = append(wm.ToolCalls, toolCallToWire(tc))
				}
			}
		case ir.RoleTool:
			wm.Role = "tool"
			for _, p := range m.Parts {
				if tr, ok := p.(ir.ToolResultPart); ok {
					wm.ToolCallID = tr.ToolUseID
					wm.Content = contentFromParts(tr.Content)
				}
			}
		}
		w.Messages = append(w.Messages, wm)
	}
	for _, t := range req.Tools {
		schema := t.InputSchema
		if len(schema) == 0 || string(schema) == "null" {
			// omitempty on an empty schema drops the field upstream —
			// a function tool without any parameters object can be
			// rejected by strict providers; {} is the documented
			// "no parameters" shape (O3).
			schema = json.RawMessage(`{}`)
		}
		w.Tools = append(w.Tools, wireTool{
			Type: "function",
			Function: wireFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  schema,
			},
		})
	}
	if req.ToolChoice != nil {
		w.ToolChoice = toolChoiceToWire(req.ToolChoice)
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("openai: encode request: %w", err)
	}
	return raw, nil
}

// DecodeResponse decodes an OpenAI chat-completions response body into the IR.
func (Codec) DecodeResponse(raw []byte) (ir.Response, error) {
	var w wireResponse
	if err := json.Unmarshal(raw, &w); err != nil {
		return ir.Response{}, fmt.Errorf("openai: decode response: %w", err)
	}
	resp := ir.Response{
		ID:    w.ID,
		Model: w.Model,
		Usage: usageFromWire(w.Usage),
	}
	if len(w.Choices) > 0 {
		resp.StopReason = stopReasonFromWire(w.Choices[0].FinishReason)
		msg := w.Choices[0].Message
		if msg != nil {
			// Reasoning trace first (reasoning models serve the trace
			// before the answer; a reasoning field on a message with no
			// trace must not fabricate an empty block).
			if r := msg.reasoningText(); r != "" {
				resp.Content = append(resp.Content, ir.ThinkingPart{Thinking: r})
			}
			if msg.Content.Str != nil && *msg.Content.Str != "" {
				resp.Content = append(resp.Content, ir.TextPart{Text: *msg.Content.Str})
			}
			for _, wt := range msg.ToolCalls {
				resp.Content = append(resp.Content, toolCallFromWire(wt))
			}
		}
	}
	return resp, nil
}

// EncodeResponse encodes an IR response into an OpenAI chat-completions body.
func (Codec) EncodeResponse(resp ir.Response) ([]byte, error) {
	w := wireResponse{
		ID:     resp.ID,
		Object: "chat.completion",
		Model:  resp.Model,
		Usage:  usageToWire(resp.Usage),
	}
	msg := &wireMessage{Role: "assistant"}
	for _, p := range resp.Content {
		switch pt := p.(type) {
		case ir.TextPart:
			msg.Content = wireContent{Str: &pt.Text}
		case ir.ToolCallPart:
			msg.ToolCalls = append(msg.ToolCalls, toolCallToWire(pt))
		}
	}
	finish := "stop"
	if resp.StopReason != "" {
		finish = stopReasonToWire(resp.StopReason)
	}
	w.Choices = []wireChoice{{
		Index:        0,
		Message:      msg,
		FinishReason: finish,
	}}
	raw, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("openai: encode response: %w", err)
	}
	return raw, nil
}

// NewStreamDecoder returns the OpenAI chat-completions SSE stream decoder for r.
func NewStreamDecoder(r io.Reader) translate.StreamDecoder {
	return newStreamDecoder(r)
}

// ---- wire types (mirror the golden fixtures) ----

// wireContent is an OpenAI message content value: a plain string, null, or
// an array of typed parts (O11).
type wireContent struct {
	Str    *string
	Parts  []wireContentPart
	IsNull bool
}

func (c *wireContent) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		c.IsNull = true
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

func (c wireContent) MarshalJSON() ([]byte, error) {
	if c.Str != nil {
		return json.Marshal(*c.Str)
	}
	if c.IsNull && c.Parts == nil {
		return []byte("null"), nil
	}
	return json.Marshal(c.Parts)
}

type wireContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *wireImageURL `json:"image_url,omitempty"`
}

type wireImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type wireToolCall struct {
	Index    int          `json:"index,omitempty"` // streaming only
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Parameters is the tool's JSON schema. omitempty would drop an empty
	// schema upstream — a strict provider (schema-less function tool) is
	// rejected; the absent-schema case normalizes to {} (schema present
	// but empty object, the OpenAI request shape for "no validation").
	Parameters json.RawMessage `json:"parameters,omitempty"`
	Arguments  string          `json:"arguments,omitempty"` // chat: JSON string (O3)
}

type wireMessage struct {
	Role    string      `json:"role"`
	Content wireContent `json:"content"`
	// Reasoning carries the reasoning trace from OpenAI-compat providers
	// that serve thinking on the chat surface. Field names vary: Ollama
	// streams `reasoning` and returns `reasoning_content` non-streaming
	// (issue ollama/ollama#17911 documents the asymmetry), DeepSeek-style
	// providers use `reasoning_content` in both. Both decode into these
	// fields; callers merge via reasoningText().
	Reasoning        string         `json:"reasoning,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	ToolCalls        []wireToolCall `json:"tool_calls,omitempty"`
}

// reasoningText returns the message's reasoning trace regardless of which
// wire field carried it (never both in practice; reasoning wins).
func (m wireMessage) reasoningText() string {
	if m.Reasoning != "" {
		return m.Reasoning
	}
	return m.ReasoningContent
}

func (m wireMessage) contentText() string {
	if m.Content.Str != nil {
		return *m.Content.Str
	}
	// Block-form system content concatenates text parts.
	var sb strings.Builder
	for _, p := range m.Content.Parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireToolChoice struct {
	// raw is the original value: a string ("auto") or an object.
	Str    *string
	Object *wireToolChoiceObject
}

type wireToolChoiceObject struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

func (c *wireToolChoice) UnmarshalJSON(b []byte) error {
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

func (c wireToolChoice) MarshalJSON() ([]byte, error) {
	if c.Str != nil {
		return json.Marshal(*c.Str)
	}
	return json.Marshal(c.Object)
}

type wireRequest struct {
	Model               string             `json:"model"`
	Messages            []wireMessage      `json:"messages"`
	Tools               []wireTool         `json:"tools,omitempty"`
	ToolChoice          *wireToolChoice    `json:"tool_choice,omitempty"`
	MaxCompletionTokens int                `json:"max_completion_tokens,omitempty"`
	MaxTokens           int                `json:"max_tokens,omitempty"`
	Temperature         *float64           `json:"temperature,omitempty"`
	TopP                *float64           `json:"top_p,omitempty"`
	Stream              bool               `json:"stream,omitempty"`
	StreamOptions       *wireStreamOptions `json:"stream_options,omitempty"`
}

// wireStreamOptions carries the OpenAI stream_options object. include_usage
// appends a final chunk whose usage object holds the turn's token counts —
// the only way a streamed response reports them.
type wireStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireUsageDetails struct {
	CachedTokens    int `json:"cached_tokens,omitempty"`
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

type wireUsage struct {
	PromptTokens            int               `json:"prompt_tokens,omitempty"`
	CompletionTokens        int               `json:"completion_tokens,omitempty"`
	TotalTokens             int               `json:"total_tokens,omitempty"`
	PromptTokensDetails     *wireUsageDetails `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *wireUsageDetails `json:"completion_tokens_details,omitempty"`
}

type wireChoice struct {
	Index        int          `json:"index"`
	Message      *wireMessage `json:"message,omitempty"`
	Delta        *wireMessage `json:"delta,omitempty"`
	FinishReason string       `json:"finish_reason"`
}

type wireResponse struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Model   string        `json:"model"`
	Choices []wireChoice  `json:"choices"`
	Usage   wireUsage     `json:"usage"`
	Error   *wireAPIError `json:"error,omitempty"`
}

type wireAPIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// ---- conversions ----

func partsFromContent(c wireContent) []ir.Part {
	var parts []ir.Part
	if c.Str != nil && *c.Str != "" {
		parts = append(parts, ir.TextPart{Text: *c.Str})
	}
	for _, p := range c.Parts {
		switch p.Type {
		case "text":
			parts = append(parts, ir.TextPart{Text: p.Text})
		case "image_url":
			if p.ImageURL == nil {
				continue
			}
			img := ir.ImagePart{Source: "url", URL: p.ImageURL.URL, Detail: p.ImageURL.Detail}
			if len(img.URL) > 5 && img.URL[:5] == "data:" {
				// data: URIs carry base64 payload inline (O11).
				img.Source = "base64"
			}
			parts = append(parts, img)
		}
	}
	return parts
}

func contentFromParts(parts []ir.Part) wireContent {
	var c wireContent
	text := ""
	hasText := false
	for _, p := range parts {
		switch pt := p.(type) {
		case ir.TextPart:
			if hasText {
				c.Parts = append(c.Parts, wireContentPart{Type: "text", Text: pt.Text})
				continue
			}
			text, hasText = pt.Text, true
		case ir.ImagePart:
			c.Parts = append(c.Parts, wireContentPart{Type: "image_url", ImageURL: &wireImageURL{
				URL:    imageURL(pt),
				Detail: pt.Detail,
			}})
		case ir.ToolCallPart:
			// Assembled by the caller into ToolCalls; not content parts.
		case ir.ToolResultPart:
			if tp, ok := anyTextPart(pt.Content); ok {
				text, hasText = tp, true
			}
		}
	}
	if len(c.Parts) > 0 {
		if hasText {
			c.Parts = append([]wireContentPart{{Type: "text", Text: text}}, c.Parts...)
		}
		return c
	}
	if hasText {
		c.Str = &text
		return c
	}
	// No encodable content (e.g. an assistant turn carrying only tool
	// calls — the standard agent-loop history shape): emit the empty
	// string, never null. Ollama's OpenAI-compatibility endpoint rejects
	// null content with HTTP 400 "invalid message content type: <nil>"
	// (ollama openai/openai.go: "invalid message content type: %T" on
	// the nil interface; mnfst/manifest#2721 reproduces it on
	// ollama-cloud, magnitudedev/magnitude#42 locally). Genuine OpenAI
	// accepts both, and partsFromContent decodes "" and null to the
	// same zero parts, so the round trip is invariant.
	empty := ""
	c.Str = &empty
	return c
}

func anyTextPart(parts []ir.Part) (string, bool) {
	for _, p := range parts {
		if tp, ok := p.(ir.TextPart); ok {
			return tp.Text, true
		}
	}
	return "", false
}

func imageURL(p ir.ImagePart) string {
	if p.Source == "base64" || p.Data != "" {
		return "data:" + p.MediaType + ";base64," + p.Data
	}
	return p.URL
}

func toolCallFromWire(w wireToolCall) ir.ToolCallPart {
	return ir.ToolCallPart{
		ID:    w.ID,
		Name:  w.Function.Name,
		Input: json.RawMessage(w.Function.Arguments),
	}
}

func toolCallToWire(tc ir.ToolCallPart) wireToolCall {
	args := string(tc.Input)
	if args == "" {
		args = "{}"
	}
	return wireToolCall{
		ID:       tc.ID,
		Type:     "function",
		Function: wireFunction{Name: tc.Name, Arguments: args},
	}
}

func toolChoiceFromWire(w *wireToolChoice) *ir.ToolChoice {
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
		return &ir.ToolChoice{Type: ir.ToolChoiceTool, Name: w.Object.Function.Name}
	}
	return nil
}

func toolChoiceToWire(tc *ir.ToolChoice) *wireToolChoice {
	switch tc.Type {
	case ir.ToolChoiceAuto, ir.ToolChoiceNone, ir.ToolChoiceAny:
		s := map[ir.ToolChoiceType]string{
			ir.ToolChoiceAuto: "auto",
			ir.ToolChoiceNone: "none",
			ir.ToolChoiceAny:  "required",
		}[tc.Type]
		return &wireToolChoice{Str: &s}
	case ir.ToolChoiceTool:
		return &wireToolChoice{Object: &wireToolChoiceObject{
			Type:     "function",
			Function: wireFunction{Name: tc.Name},
		}}
	default:
		return nil
	}
}

func usageFromWire(w wireUsage) ir.Usage {
	u := ir.Usage{
		InputTokens:  w.PromptTokens,
		OutputTokens: w.CompletionTokens,
	}
	if w.PromptTokensDetails != nil {
		u.CacheReadInputTokens = w.PromptTokensDetails.CachedTokens
	}
	if w.CompletionTokensDetails != nil {
		u.ReasoningTokens = w.CompletionTokensDetails.ReasoningTokens
	}
	return u
}

func usageToWire(u ir.Usage) wireUsage {
	w := wireUsage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
	}
	if u.CacheReadInputTokens != 0 {
		w.PromptTokensDetails = &wireUsageDetails{CachedTokens: u.CacheReadInputTokens}
	}
	if u.ReasoningTokens != 0 {
		w.CompletionTokensDetails = &wireUsageDetails{ReasoningTokens: u.ReasoningTokens}
	}
	return w
}

func stopReasonFromWire(s string) ir.StopReason {
	switch s {
	case "stop":
		return ir.StopEndTurn
	case "length":
		return ir.StopMaxTokens
	case "tool_calls", "function":
		return ir.StopToolUse
	case "content_filter":
		return ir.StopContentFilter
	case "refusal":
		return ir.StopRefusal
	default:
		return ""
	}
}

func stopReasonToWire(s ir.StopReason) string {
	switch s {
	case ir.StopEndTurn:
		return "stop"
	case ir.StopMaxTokens:
		return "length"
	case ir.StopToolUse:
		return "tool_calls"
	case ir.StopContentFilter:
		return "content_filter"
	case ir.StopRefusal:
		return "refusal"
	case ir.StopStopSequence:
		return "stop"
	default:
		return "stop"
	}
}
