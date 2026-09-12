// Package anthropic implements the Anthropic Messages API codec pair against
// the canonical IR (plan Analysis D). Wire shapes derive from the golden
// fixtures in testdata/golden/anthropic/ (docs.anthropic.com, accessed
// 2026-09-10, facts A1–A13).
package anthropic

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

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
		req.Tools = append(req.Tools, toolFromWire(wt))
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
		w.Tools = append(w.Tools, toolToWire(t))
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
		Usage:        w.Usage.toIR(),
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
		Usage:        usageToWire(resp.Usage),
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
		case ir.ServerToolUsePart:
			w.Content = append(w.Content, wireBlock{
				Type:  "server_tool_use",
				ID:    pt.ID,
				Name:  pt.Name,
				Input: pt.Input,
			})
		case ir.ServerToolResultPart:
			w.Content = append(w.Content, serverToolResultToWire(pt))
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

// wireContent is an Anthropic content value: a plain string, an array of
// typed blocks (A3), or — for server-tool result blocks — a single block
// object (web_fetch's single web_fetch_result; both tools' error shapes).
type wireContent struct {
	Str    *string
	Blocks []wireBlock
	Obj    *wireBlock
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
	if len(b) > 0 && b[0] == '{' {
		// Single-object content: server-tool result blocks (web_fetch's
		// web_fetch_result, both tools' *_tool_result_error shapes —
		// verified live 2026-09-12).
		var blk wireBlock
		if err := json.Unmarshal(b, &blk); err != nil {
			return err
		}
		c.Obj = &blk
		return nil
	}
	return json.Unmarshal(b, &c.Blocks)
}

func (c *wireContent) MarshalJSON() ([]byte, error) {
	if c.Str != nil {
		return json.Marshal(*c.Str)
	}
	if c.Obj != nil {
		return json.Marshal(*c.Obj)
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
	ErrorCode    string            `json:"error_code,omitempty"`
	URL          string            `json:"url,omitempty"`
	Title        string            `json:"title,omitempty"`
	// EncryptedContent is web_search_result's opaque provider payload.
	EncryptedContent string         `json:"encrypted_content,omitempty"`
	PageAge          string         `json:"page_age,omitempty"`
	RetrievedAt      string         `json:"retrieved_at,omitempty"`
	Citations        *wireCitations `json:"citations,omitempty"`
}

type wireMessage struct {
	Role    string       `json:"role"`
	Content *wireContent `json:"content"`
}

// wireTool is a client tool definition (function-shaped) or a server-tool
// definition (A8: dated type string + knobs). Server-tool defs carry no
// input_schema.
type wireTool struct {
	Type             string            `json:"type,omitempty"` // server tools only
	Name             string            `json:"name"`
	Description      string            `json:"description,omitempty"`
	InputSchema      json.RawMessage   `json:"input_schema,omitempty"`
	MaxUses          int               `json:"max_uses,omitempty"`
	AllowedDomains   []string          `json:"allowed_domains,omitempty"`
	BlockedDomains   []string          `json:"blocked_domains,omitempty"`
	UserLocation     *wireUserLocation `json:"user_location,omitempty"`
	Citations        *wireCitations    `json:"citations,omitempty"`
	MaxContentTokens int               `json:"max_content_tokens,omitempty"`
}

type wireUserLocation struct {
	Type     string `json:"type"`
	City     string `json:"city,omitempty"`
	Region   string `json:"region,omitempty"`
	Country  string `json:"country,omitempty"`
	Timezone string `json:"timezone,omitempty"`
}

type wireCitations struct {
	Enabled bool `json:"enabled"`
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
	Usage        wireUsage     `json:"usage"`
}

// wireUsage mirrors the Anthropic usage object (A11 + A8 server-tool
// counters): cache fields are explicit zeros on the wire, server-tool
// counters nest under server_tool_use.
type wireUsage struct {
	InputTokens              int                `json:"input_tokens"`
	OutputTokens             int                `json:"output_tokens"`
	CacheCreationInputTokens int                `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int                `json:"cache_read_input_tokens"`
	ServerToolUse            *wireServerToolUse `json:"server_tool_use,omitempty"`
}

type wireServerToolUse struct {
	WebSearchRequests int `json:"web_search_requests,omitempty"`
	WebFetchRequests  int `json:"web_fetch_requests,omitempty"`
}

func (w wireUsage) toIR() ir.Usage {
	u := ir.Usage{
		InputTokens:              w.InputTokens,
		OutputTokens:             w.OutputTokens,
		CacheCreationInputTokens: w.CacheCreationInputTokens,
		CacheReadInputTokens:     w.CacheReadInputTokens,
	}
	if w.ServerToolUse != nil {
		u.WebSearchRequests = w.ServerToolUse.WebSearchRequests
		u.WebFetchRequests = w.ServerToolUse.WebFetchRequests
	}
	return u
}

func usageToWire(u ir.Usage) wireUsage {
	w := wireUsage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
	}
	if u.WebSearchRequests != 0 || u.WebFetchRequests != 0 {
		w.ServerToolUse = &wireServerToolUse{
			WebSearchRequests: u.WebSearchRequests,
			WebFetchRequests:  u.WebFetchRequests,
		}
	}
	return w
}

// ---- conversions ----

// toolFromWire converts a wire tool definition. Server-tool defs (A8) keep
// their dated type string verbatim (C3 open-list ride-along) and surface
// their knobs via ServerTool; client tools stay function-shaped.
func toolFromWire(w wireTool) ir.Tool {
	t := ir.Tool{
		Name:        w.Name,
		Description: w.Description,
		InputSchema: w.InputSchema,
		Type:        w.Type,
	}
	if w.Type != "" {
		st := &ir.ServerToolConfig{
			MaxUses:          w.MaxUses,
			AllowedDomains:   w.AllowedDomains,
			BlockedDomains:   w.BlockedDomains,
			CitationsEnabled: w.Citations != nil && w.Citations.Enabled,
			MaxContentTokens: w.MaxContentTokens,
		}
		if w.UserLocation != nil {
			st.UserLocation = &ir.UserLocation{
				Type:     w.UserLocation.Type,
				City:     w.UserLocation.City,
				Region:   w.UserLocation.Region,
				Country:  w.UserLocation.Country,
				Timezone: w.UserLocation.Timezone,
			}
		}
		t.ServerTool = st
	}
	return t
}

// toolToWire re-encodes a tool definition. Server-tool knobs ride back out
// only when the IR carries a ServerTool config (a bare Type string on a
// degraded cross-protocol tool def emits just type+name).
func toolToWire(t ir.Tool) wireTool {
	w := wireTool{
		Type:        t.Type,
		Name:        t.Name,
		Description: t.Description,
		InputSchema: t.InputSchema,
	}
	if st := t.ServerTool; st != nil {
		w.MaxUses = st.MaxUses
		w.AllowedDomains = st.AllowedDomains
		w.BlockedDomains = st.BlockedDomains
		w.MaxContentTokens = st.MaxContentTokens
		if st.CitationsEnabled {
			w.Citations = &wireCitations{Enabled: true}
		}
		if st.UserLocation != nil {
			w.UserLocation = &wireUserLocation{
				Type:     st.UserLocation.Type,
				City:     st.UserLocation.City,
				Region:   st.UserLocation.Region,
				Country:  st.UserLocation.Country,
				Timezone: st.UserLocation.Timezone,
			}
		}
	}
	return w
}

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
	case "server_tool_use":
		return ir.ServerToolUsePart{ID: b.ID, Name: b.Name, Input: b.Input}, true
	case "web_search_tool_result", "web_fetch_tool_result":
		res := ir.ServerToolResultPart{ToolUseID: b.ToolUseID}
		if b.Type == "web_search_tool_result" {
			res.ToolName = "web_search"
		} else {
			res.ToolName = "web_fetch"
		}
		if b.Content == nil {
			return res, true
		}
		switch {
		case b.Content.Obj != nil && strings.HasSuffix(b.Content.Obj.Type, "_tool_result_error"):
			// Error shape: single error object with error_code (A10).
			res.IsError = true
			res.ErrorCode = b.Content.Obj.ErrorCode
		case b.Content.Obj != nil:
			// web_fetch success: single web_fetch_result object with a
			// nested document content block (source text|base64). Title
			// and citations live on the nested document, not the outer
			// result object (verified live 2026-09-12).
			obj := b.Content.Obj
			fc := &ir.WebFetchContent{
				URL:         obj.URL,
				RetrievedAt: obj.RetrievedAt,
			}
			if obj.Content != nil && obj.Content.Obj != nil {
				doc := obj.Content.Obj
				fc.Title = doc.Title
				fc.CitationsEnabled = doc.Citations != nil && doc.Citations.Enabled
				if doc.Source != nil {
					fc.Source = doc.Source.Type
					fc.MediaType = doc.Source.MediaType
					fc.Data = doc.Source.Data
				}
			}
			res.Fetch = fc
		default:
			// web_search success: array of web_search_result blocks.
			for _, cb := range b.Content.Blocks {
				if cb.Type != "web_search_result" {
					continue
				}
				res.Search = append(res.Search, ir.WebSearchResultItem{
					URL:              cb.URL,
					Title:            cb.Title,
					EncryptedContent: cb.EncryptedContent,
					PageAge:          cb.PageAge,
				})
			}
		}
		return res, true
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

// serverToolResultToWire encodes a ServerToolResultPart back into its wire
// shape, reproducing the asymmetry: web_search content is an ARRAY of
// web_search_result blocks, web_fetch content a SINGLE result object, and
// errors a single *_tool_result_error object (verified live 2026-09-12).
func serverToolResultToWire(p ir.ServerToolResultPart) wireBlock {
	blkType := "web_search_tool_result"
	if p.ToolName == "web_fetch" {
		blkType = "web_fetch_tool_result"
	}
	b := wireBlock{Type: blkType, ToolUseID: p.ToolUseID}
	switch {
	case p.IsError:
		errType := "web_search_tool_result_error"
		if p.ToolName == "web_fetch" {
			errType = "web_fetch_tool_result_error"
		}
		b.Content = &wireContent{Obj: &wireBlock{Type: errType, ErrorCode: p.ErrorCode}}
	case p.ToolName == "web_fetch" && p.Fetch != nil:
		res := &wireBlock{
			Type:        "web_fetch_result",
			URL:         p.Fetch.URL,
			RetrievedAt: p.Fetch.RetrievedAt,
		}
		doc := &wireBlock{
			Type:   "document",
			Title:  p.Fetch.Title,
			Source: &wireImageSource{Type: p.Fetch.Source, MediaType: p.Fetch.MediaType, Data: p.Fetch.Data},
		}
		if p.Fetch.CitationsEnabled {
			doc.Citations = &wireCitations{Enabled: true}
		}
		res.Content = &wireContent{Obj: doc}
		b.Content = &wireContent{Obj: res}
	default:
		content := &wireContent{Blocks: []wireBlock{}}
		for _, item := range p.Search {
			content.Blocks = append(content.Blocks, wireBlock{
				Type:             "web_search_result",
				URL:              item.URL,
				Title:            item.Title,
				EncryptedContent: item.EncryptedContent,
				PageAge:          item.PageAge,
			})
		}
		b.Content = content
	}
	return b
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
	case ir.ServerToolUsePart:
		return wireBlock{Type: "server_tool_use", ID: pt.ID, Name: pt.Name, Input: pt.Input}
	case ir.ServerToolResultPart:
		return serverToolResultToWire(pt)
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
