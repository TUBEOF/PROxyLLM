// Package anthropic implements the Anthropic Messages API codec pair against
// the canonical IR (plan Analysis D). Wire shapes derive from the golden
// fixtures in testdata/golden/anthropic/ (docs.anthropic.com, accessed
// 2026-09-10, facts A1–A13).
package anthropic

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/proxyllm/proxyllm/internal/translate"
	"github.com/proxyllm/proxyllm/internal/translate/ir"
)

// Codec decodes and encodes Anthropic Messages wire bodies.
type Codec struct{}

// DecodeRequest decodes an Anthropic Messages request body into the IR.
func (Codec) DecodeRequest(raw []byte) (ir.Request, error) {
	var w wireRequest
	if err := json.Unmarshal(raw, &w); err != nil {
		return ir.Request{}, fmt.Errorf("anthropic: decode request: %w", err)
	}
	req := ir.Request{
		Model:         w.Model,
		MaxTokens:     w.MaxTokens,
		Temperature:   w.Temperature,
		TopP:          w.TopP,
		StopSequences: w.StopSequences,
		Stream:        w.Stream,
	}
	if w.Thinking != nil {
		req.Thinking = &ir.ThinkingConfig{
			Enabled:      w.Thinking.Type == "enabled",
			BudgetTokens: w.Thinking.BudgetTokens,
			Adaptive:     w.Thinking.Type == "adaptive",
		}
	}
	if w.System != nil {
		if w.System.Str != nil && *w.System.Str != "" {
			req.System = append(req.System, ir.TextPart{Text: *w.System.Str})
		}
		for _, b := range w.System.Blocks {
			if b.Type == "text" {
				req.System = append(req.System, ir.TextPart{
					Text:         b.Text,
					CacheControl: cacheControlFromWire(b.CacheControl),
				})
			}
		}
	}
	for _, wm := range w.Messages {
		msg := ir.Message{Role: ir.Role(wm.Role)}
		if wm.Content != nil {
			if wm.Content.Str != nil && *wm.Content.Str != "" {
				msg.Parts = append(msg.Parts, ir.TextPart{Text: *wm.Content.Str})
			}
			for _, b := range wm.Content.Blocks {
				if p, ok := partFromBlock(b); ok {
					msg.Parts = append(msg.Parts, p)
				}
			}
		}
		req.Messages = append(req.Messages, msg)
	}
	for _, wt := range w.Tools {
		req.Tools = append(req.Tools, ir.Tool{
			Name:        wt.Name,
			Description: wt.Description,
			InputSchema: wt.InputSchema,
		})
	}
	if w.ToolChoice != nil {
		req.ToolChoice = toolChoiceFromWire(w.ToolChoice)
	}
	return req, nil
}

// EncodeRequest encodes an IR request into an Anthropic Messages body.
func (Codec) EncodeRequest(req ir.Request) ([]byte, error) {
	w := wireRequest{
		Model:         req.Model,
		MaxTokens:     req.MaxTokens,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		StopSequences: req.StopSequences,
		Stream:        req.Stream,
	}
	if len(req.System) > 0 {
		sys := &wireContent{}
		for _, tp := range req.System {
			sys.Blocks = append(sys.Blocks, wireBlock{
				Type:         "text",
				Text:         tp.Text,
				CacheControl: cacheControlToWire(tp.CacheControl),
			})
		}
		w.System = sys
	}
	for _, m := range req.Messages {
		role := string(m.Role)
		if m.Role == ir.RoleTool {
			// Anthropic has no tool role; tool results ride in user
			// messages as tool_result blocks (A3).
			role = "user"
		}
		wm := wireMessage{Role: role, Content: &wireContent{}}
		for _, p := range m.Parts {
			wm.Content.Blocks = append(wm.Content.Blocks, blockFromPart(p))
		}
		w.Messages = append(w.Messages, wm)
	}
	for _, t := range req.Tools {
		w.Tools = append(w.Tools, wireTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	if req.ToolChoice != nil {
		w.ToolChoice = toolChoiceToWire(req.ToolChoice)
	}
	if req.Thinking != nil {
		switch {
		case req.Thinking.Adaptive:
			w.Thinking = &wireThinking{Type: "adaptive"}
		case req.Thinking.Enabled:
			w.Thinking = &wireThinking{Type: "enabled", BudgetTokens: req.Thinking.BudgetTokens}
		}
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("anthropic: encode request: %w", err)
	}
	return raw, nil
}

// DecodeResponse decodes an Anthropic Messages response body into the IR.
func (Codec) DecodeResponse(raw []byte) (ir.Response, error) {
	var w wireResponse
	if err := json.Unmarshal(raw, &w); err != nil {
		return ir.Response{}, fmt.Errorf("anthropic: decode response: %w", err)
	}
	resp := ir.Response{
		ID:           w.ID,
		Model:        w.Model,
		StopReason:   w.StopReason,
		StopSequence: w.StopSequence,
		Usage:        w.Usage,
	}
	for _, b := range w.Content {
		if p, ok := partFromBlock(b); ok {
			resp.Content = append(resp.Content, p)
		}
	}
	return resp, nil
}

// EncodeResponse encodes an IR response into an Anthropic Messages body.
func (Codec) EncodeResponse(resp ir.Response) ([]byte, error) {
	w := wireResponse{
		ID:           resp.ID,
		Type:         "message",
		Role:         "assistant",
		Model:        resp.Model,
		StopReason:   resp.StopReason,
		StopSequence: resp.StopSequence,
		Usage:        resp.Usage,
	}
	for _, p := range resp.Content {
		switch pt := p.(type) {
		case ir.TextPart:
			w.Content = append(w.Content, wireBlock{Type: "text", Text: pt.Text})
		case ir.ThinkingPart:
			// Signatures ride verbatim — never fabricated (A12).
			w.Content = append(w.Content, wireBlock{
				Type:      "thinking",
				Thinking:  pt.Thinking,
				Signature: pt.Signature,
			})
		case ir.ToolCallPart:
			w.Content = append(w.Content, wireBlock{
				Type:  "tool_use",
				ID:    pt.ID,
				Name:  pt.Name,
				Input: pt.Input,
			})
		}
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("anthropic: encode response: %w", err)
	}
	return raw, nil
}

// NewStreamDecoder returns the Anthropic SSE stream decoder for r.
func NewStreamDecoder(r io.Reader) translate.StreamDecoder {
	return newStreamDecoder(r)
}

// ---- wire types (mirror the golden fixtures) ----

type wireCacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

type wireImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// wireContent is an Anthropic content value: a plain string or an array of
// typed blocks (A3).
type wireContent struct {
	Str    *string
	Blocks []wireBlock
}

func (c *wireContent) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		c.Str = &s
		return nil
	}
	if string(b) == "null" {
		return nil
	}
	return json.Unmarshal(b, &c.Blocks)
}

func (c *wireContent) MarshalJSON() ([]byte, error) {
	if c.Str != nil {
		return json.Marshal(*c.Str)
	}
	if c.Blocks == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(c.Blocks)
}

type wireBlock struct {
	Type         string            `json:"type"`
	Text         string            `json:"text,omitempty"`
	Thinking     string            `json:"thinking,omitempty"`
	Signature    string            `json:"signature,omitempty"`
	ID           string            `json:"id,omitempty"`
	Name         string            `json:"name,omitempty"`
	Input        json.RawMessage   `json:"input,omitempty"`
	ToolUseID    string            `json:"tool_use_id,omitempty"`
	Content      *wireContent      `json:"content,omitempty"`
	IsError      bool              `json:"is_error,omitempty"`
	CacheControl *wireCacheControl `json:"cache_control,omitempty"`
	Source       *wireImageSource  `json:"source,omitempty"`
}

type wireMessage struct {
	Role    string       `json:"role"`
	Content *wireContent `json:"content"`
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type wireToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type wireThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type wireRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Thinking      *wireThinking   `json:"thinking,omitempty"`
	System        *wireContent    `json:"system,omitempty"`
	Messages      []wireMessage   `json:"messages"`
	Tools         []wireTool      `json:"tools,omitempty"`
	ToolChoice    *wireToolChoice `json:"tool_choice,omitempty"`
}

type wireResponse struct {
	ID           string        `json:"id"`
	Type         string        `json:"type"`
	Role         string        `json:"role"`
	Model        string        `json:"model"`
	Content      []wireBlock   `json:"content"`
	StopReason   ir.StopReason `json:"stop_reason,omitempty"`
	StopSequence string        `json:"stop_sequence,omitempty"`
	Usage        ir.Usage      `json:"usage"`
}

// ---- conversions ----

func cacheControlFromWire(w *wireCacheControl) *ir.CacheControl {
	if w == nil {
		return nil
	}
	return &ir.CacheControl{Type: w.Type, TTL: w.TTL}
}

func cacheControlToWire(c *ir.CacheControl) *wireCacheControl {
	if c == nil {
		return nil
	}
	return &wireCacheControl{Type: c.Type, TTL: c.TTL}
}

func toolChoiceFromWire(w *wireToolChoice) *ir.ToolChoice {
	tc := &ir.ToolChoice{Type: ir.ToolChoiceType(w.Type)}
	switch tc.Type {
	case ir.ToolChoiceAuto, ir.ToolChoiceAny, ir.ToolChoiceNone:
	case ir.ToolChoiceTool:
		tc.Name = w.Name
	default:
		// Unknown choice types ride through as raw types; routing
		// validates against the model table.
	}
	return tc
}

func toolChoiceToWire(tc *ir.ToolChoice) *wireToolChoice {
	return &wireToolChoice{Type: string(tc.Type), Name: tc.Name}
}

// partFromBlock converts a wire content block into an IR part. Unknown block
// types report ok=false: the IR has no representation yet (server-tool
// blocks, A8) and they drop with a documented degradation — passthrough
// lands with the routing layer per plan Analysis D.
func partFromBlock(b wireBlock) (ir.Part, bool) {
	switch b.Type {
	case "text":
		return ir.TextPart{Text: b.Text, CacheControl: cacheControlFromWire(b.CacheControl)}, true
	case "thinking":
		return ir.ThinkingPart{Thinking: b.Thinking, Signature: b.Signature}, true
	case "image":
		if b.Source == nil {
			return nil, false
		}
		return ir.ImagePart{
			Source:    b.Source.Type, // "base64" | "url"
			MediaType: b.Source.MediaType,
			Data:      b.Source.Data,
			URL:       b.Source.URL,
		}, true
	case "tool_use":
		return ir.ToolCallPart{ID: b.ID, Name: b.Name, Input: b.Input}, true
	case "tool_result":
		tr := ir.ToolResultPart{
			ToolUseID:    b.ToolUseID,
			IsError:      b.IsError,
			CacheControl: cacheControlFromWire(b.CacheControl),
		}
		if b.Content != nil {
			if b.Content.Str != nil && *b.Content.Str != "" {
				tr.Content = append(tr.Content, ir.TextPart{Text: *b.Content.Str})
			} else {
				for _, cb := range b.Content.Blocks {
					if p, ok := partFromBlock(cb); ok {
						tr.Content = append(tr.Content, p)
					}
				}
			}
		}
		return tr, true
	default:
		return nil, false
	}
}

func blockFromPart(p ir.Part) wireBlock {
	switch pt := p.(type) {
	case ir.TextPart:
		return wireBlock{Type: "text", Text: pt.Text, CacheControl: cacheControlToWire(pt.CacheControl)}
	case ir.ThinkingPart:
		return wireBlock{Type: "thinking", Thinking: pt.Thinking, Signature: pt.Signature}
	case ir.ImagePart:
		return wireBlock{Type: "image", Source: &wireImageSource{
			Type:      pt.Source,
			MediaType: pt.MediaType,
			Data:      pt.Data,
			URL:       pt.URL,
		}}
	case ir.ToolCallPart:
		return wireBlock{Type: "tool_use", ID: pt.ID, Name: pt.Name, Input: pt.Input}
	case ir.ToolResultPart:
		content := &wireContent{}
		if len(pt.Content) == 1 {
			if tp, ok := pt.Content[0].(ir.TextPart); ok && tp.CacheControl == nil {
				content.Str = &tp.Text
				return wireBlock{
					Type:         "tool_result",
					ToolUseID:    pt.ToolUseID,
					Content:      content,
					IsError:      pt.IsError,
					CacheControl: cacheControlToWire(pt.CacheControl),
				}
			}
		}
		for _, cp := range pt.Content {
			content.Blocks = append(content.Blocks, blockFromPart(cp))
		}
		return wireBlock{
			Type:         "tool_result",
			ToolUseID:    pt.ToolUseID,
			Content:      content,
			IsError:      pt.IsError,
			CacheControl: cacheControlToWire(pt.CacheControl),
		}
	default:
		return wireBlock{Type: "text"}
	}
}
