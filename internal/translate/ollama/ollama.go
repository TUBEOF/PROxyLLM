// Package ollama implements the Ollama native /api/chat codec pair
// against the canonical IR. Wire shapes derive from the
// golden fixtures in testdata/golden/ollama/ (docs.ollama.com, accessed
// 2026-09-10).
package ollama

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"github.com/tubeof/proxyllm/internal/translate"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// Codec decodes and encodes Ollama /api/chat wire bodies.
type Codec struct{}

// DecodeRequest decodes an Ollama /api/chat request body into the IR.
// The system role message decodes into System[0]; tools are
// OpenAI-function-shaped; the think parameter accepts bool or an
// effort string.
func (Codec) DecodeRequest(raw []byte) (ir.Request, error) {
	var w wireRequest
	if err := json.Unmarshal(raw, &w); err != nil {
		return ir.Request{}, fmt.Errorf("ollama: decode request: %w", err)
	}
	req := ir.Request{
		Model:          w.Model,
		Stream:         w.Stream,
		KeepAlive:      w.KeepAlive,
		ResponseFormat: w.Format,
		SourceProtocol: "ollama",
		Passthrough:    ir.UnknownFields(raw, ollamaRequestFields),
	}
	if w.Options != nil {
		if v, ok := w.Options["temperature"].(float64); ok {
			req.Temperature = &v
		}
		if v, ok := w.Options["top_p"].(float64); ok {
			req.TopP = &v
		}
		if v, ok := w.Options["top_k"].(float64); ok {
			i := int(v)
			req.TopK = &i
		}
		if v, ok := w.Options["num_predict"].(float64); ok {
			req.MaxTokens = int(v)
		}
		if v, ok := w.Options["seed"].(float64); ok {
			i := int(v)
			req.Seed = &i
		}
		if v, ok := w.Options["stop"].([]any); ok {
			for _, s := range v {
				if text, ok := s.(string); ok {
					req.StopSequences = append(req.StopSequences, text)
				}
			}
		}
	}
	if rawOpts, ok := rawField(raw, "options"); ok {
		var m map[string]json.RawMessage
		if json.Unmarshal(rawOpts, &m) == nil {
			req.OllamaOptions = m
		}
	}
	if w.Think != nil && w.Think.on() {
		// Ollama thinking levels (high/medium/low/max) or true map onto the
		// IR enabled flag; a budget number is not expressible on the wire
		// side, so decode stays qualitative.
		req.Thinking = &ir.ThinkingConfig{Enabled: true}
	}
	for _, wm := range w.Messages {
		switch wm.Role {
		case "system":
			req.System = append(req.System, ir.TextPart{Text: wm.Content})
		case "user":
			msg := ir.Message{Role: ir.RoleUser}
			if wm.Content != "" {
				msg.Parts = append(msg.Parts, ir.TextPart{Text: wm.Content})
			}
			for _, img := range wm.Images {
				// Ollama carries images as raw base64 strings.
				msg.Parts = append(msg.Parts, ir.ImagePart{Source: "base64", Data: img})
			}
			req.Messages = append(req.Messages, msg)
		case "assistant":
			msg := ir.Message{Role: ir.RoleAssistant}
			if wm.Thinking != "" {
				msg.Parts = append(msg.Parts, ir.ThinkingPart{Thinking: wm.Thinking})
			}
			if wm.Content != "" {
				msg.Parts = append(msg.Parts, ir.TextPart{Text: wm.Content})
			}
			for _, wtc := range wm.ToolCalls {
				msg.Parts = append(msg.Parts, ir.ToolCallPart{
					Name:  wtc.Function.Name,
					Input: wtc.Function.Arguments,
				})
			}
			req.Messages = append(req.Messages, msg)
		case "tool":
			tr := ir.ToolResultPart{}
			if wm.Content != "" {
				tr.Content = append(tr.Content, ir.TextPart{Text: wm.Content})
			}
			// The Message schema's images field is role-agnostic (docs/api.md,
			// "a list of images to include in the message"): a tool result may
			// carry the pixels a tool produced (matrix catalog №1).
			for _, img := range wm.Images {
				tr.Content = append(tr.Content, ir.ImagePart{Source: "base64", Data: img})
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
	return req, nil
}

// EncodeRequest encodes an IR request into an Ollama /api/chat body.
// stream is always emitted explicitly — Ollama defaults stream to true when
// the field is absent. Thinking maps to the think parameter:
// adaptive → true, enabled with a budget → the closest effort level.
func (Codec) EncodeRequest(req ir.Request) ([]byte, error) {
	w := wireRequest{
		Model:     req.Model,
		Stream:    req.Stream, // no omitempty: stream:false must be explicit
		KeepAlive: req.KeepAlive,
		Format:    ollamaFormat(req.ResponseFormat),
	}
	opts := map[string]any{}
	if req.Temperature != nil {
		opts["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		opts["top_p"] = *req.TopP
	}
	if req.TopK != nil {
		opts["top_k"] = *req.TopK
	}
	if req.MaxTokens > 0 {
		opts["num_predict"] = req.MaxTokens
	}
	if req.Seed != nil {
		opts["seed"] = *req.Seed
	}
	if len(req.StopSequences) > 0 {
		opts["stop"] = req.StopSequences
	}
	for k, rawv := range req.OllamaOptions {
		if _, exists := opts[k]; exists {
			continue
		}
		var v any
		if json.Unmarshal(rawv, &v) == nil {
			opts[k] = v
		}
	}
	if len(opts) > 0 {
		w.Options = opts
	}
	if req.Thinking != nil && req.Thinking.Enabled {
		switch {
		case req.Thinking.Adaptive:
			w.Think = &wireThink{Bool: boolPtr(true)}
		default:
			w.Think = &wireThink{Str: strPtr(effortFor(req))}
		}
	} else if req.ReasoningEffort != "" {
		effort := req.ReasoningEffort
		if effort == "minimal" {
			effort = "low"
		}
		w.Think = &wireThink{Str: strPtr(effort)}
	}
	if len(req.System) > 0 {
		sys := wireMessage{Role: "system"}
		for _, tp := range req.System {
			sys.Content += tp.Text
		}
		w.Messages = append(w.Messages, sys)
	}
	for _, m := range req.Messages {
		wm := wireMessage{Role: string(m.Role)}
		switch m.Role {
		case ir.RoleTool:
			// Ollama tool results are role:"tool" messages with text
			// content — and images ride the message's role-agnostic
			// images field (docs/api.md; matrix catalog №1): a tool that
			// produced pixels must not lose them in transit.
			for _, p := range m.Parts {
				if tr, ok := p.(ir.ToolResultPart); ok {
					for _, cp := range tr.Content {
						if tp, ok := cp.(ir.TextPart); ok {
							wm.Content += tp.Text
						}
						if img, ok := cp.(ir.ImagePart); ok && img.Source == "base64" {
							wm.Images = append(wm.Images, img.Data)
						}
					}
				}
			}
		default:
			for _, p := range m.Parts {
				switch pt := p.(type) {
				case ir.TextPart:
					wm.Content += pt.Text
				case ir.ImagePart:
					if pt.Source == "base64" {
						wm.Images = append(wm.Images, pt.Data)
					}
				case ir.ThinkingPart:
					wm.Thinking = pt.Thinking
				case ir.ToolCallPart:
					wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
						Function: wireToolCallFunction{
							Name:      pt.Name,
							Arguments: json.RawMessage(pt.Input),
						},
					})
				}
			}
		}
		w.Messages = append(w.Messages, wm)
	}
	for _, t := range req.Tools {
		w.Tools = append(w.Tools, wireTool{
			Type: "function",
			Function: wireToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("ollama: encode request: %w", err)
	}
	return ir.MergePassthrough(raw, ir.PassthroughFor(req, "ollama"))
}

// DecodeResponse decodes an Ollama /api/chat response body into the IR.
// Ollama responses carry no ID — one is synthesized from model+created_at.
// done_reason normalizes to the IR stop reason (its value strings are only
// loosely pinned, so an unrecognized value falls back to end_turn). Usage
// maps prompt_eval_count/eval_count/prompt_eval_cached_count.
func (Codec) DecodeResponse(raw []byte) (ir.Response, error) {
	var w wireResponse
	if err := json.Unmarshal(raw, &w); err != nil {
		return ir.Response{}, fmt.Errorf("ollama: decode response: %w", err)
	}
	if w.Error != "" {
		return ir.Response{}, fmt.Errorf("ollama: upstream error: %s", w.Error)
	}
	resp := ir.Response{
		ID:         synthesizeID(w.Model, w.CreatedAt),
		Model:      w.Model,
		StopReason: stopReasonFromWire(w.DoneReason),
		Usage:      usageFromWire(w),
	}
	if w.Message.Thinking != "" {
		resp.Content = append(resp.Content, ir.ThinkingPart{Thinking: w.Message.Thinking})
	}
	if w.Message.Content != "" {
		resp.Content = append(resp.Content, ir.TextPart{Text: w.Message.Content})
	}
	for _, wtc := range w.Message.ToolCalls {
		// Ollama tool_calls arguments are JSON objects, not strings.
		resp.Content = append(resp.Content, ir.ToolCallPart{
			Name:  wtc.Function.Name,
			Input: wtc.Function.Arguments,
		})
	}
	return resp, nil
}

// EncodeResponse encodes an IR response into an Ollama /api/chat body.
func (Codec) EncodeResponse(resp ir.Response) ([]byte, error) {
	w := wireResponse{
		Model:      resp.Model,
		Done:       true,
		DoneReason: stopReasonToWire(resp.StopReason),
	}
	w.Message.Role = "assistant"
	for _, p := range resp.Content {
		switch pt := p.(type) {
		case ir.TextPart:
			w.Message.Content += pt.Text
		case ir.ThinkingPart:
			w.Message.Thinking = pt.Thinking
		case ir.ToolCallPart:
			w.Message.ToolCalls = append(w.Message.ToolCalls, wireToolCall{
				Function: wireToolCallFunction{
					Name:      pt.Name,
					Arguments: json.RawMessage(pt.Input),
				},
			})
		}
	}
	w.PromptEvalCount = resp.Usage.InputTokens
	w.EvalCount = resp.Usage.OutputTokens
	w.PromptEvalCachedCount = resp.Usage.CacheReadInputTokens
	raw, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("ollama: encode response: %w", err)
	}
	return raw, nil
}

// ---- wire types (mirror the golden fixtures) ----

// wireThink is the think parameter: bool or an effort string
// (high/medium/low/max).
type wireThink struct {
	Bool *bool
	Str  *string
}

func (t *wireThink) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		t.Str = &s
		return nil
	}
	var v bool
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	t.Bool = &v
	return nil
}

func (t wireThink) MarshalJSON() ([]byte, error) {
	if t.Str != nil {
		return json.Marshal(*t.Str)
	}
	if t.Bool != nil {
		return json.Marshal(*t.Bool)
	}
	return []byte("false"), nil
}

// on reports whether the decoded think parameter enables thinking.
func (t *wireThink) on() bool {
	if t == nil {
		return false
	}
	return (t.Bool != nil && *t.Bool) || (t.Str != nil && *t.Str != "" && *t.Str != "false")
}

type wireMessage struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	Images    []string       `json:"images,omitempty"`
	Thinking  string         `json:"thinking,omitempty"`
	ToolCalls []wireToolCall `json:"tool_calls,omitempty"`
}

// wireToolCall mirrors Ollama's response tool_calls entries: function-shaped
// with OBJECT arguments. Requests never carry tool_calls entries.
type wireToolCall struct {
	Function wireToolCallFunction `json:"function"`
}

type wireToolCallFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Arguments   json.RawMessage `json:"arguments,omitempty"`
}

// wireTool is the request tool definition, OpenAI-function-shaped.
type wireTool struct {
	Type     string           `json:"type"`
	Function wireToolFunction `json:"function"`
}

type wireToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type wireRequest struct {
	Model     string          `json:"model"`
	Messages  []wireMessage   `json:"messages"`
	Stream    bool            `json:"stream"` // always explicit, never omitted
	Think     *wireThink      `json:"think,omitempty"`
	Tools     []wireTool      `json:"tools,omitempty"`
	Options   map[string]any  `json:"options,omitempty"`
	KeepAlive json.RawMessage `json:"keep_alive,omitempty"`
	Format    json.RawMessage `json:"format,omitempty"`
}

// UnmarshalJSON defaults stream to true: Ollama's /api/chat streams unless the
// client explicitly sends stream:false. A plain bool would decode an absent
// field as false and turn a stock Ollama client into a non-streaming one.
func (w *wireRequest) UnmarshalJSON(b []byte) error {
	type alias wireRequest
	aux := alias{Stream: true}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	*w = wireRequest(aux)
	return nil
}

// rawField returns one top-level member of a JSON object.
func rawField(raw []byte, name string) (json.RawMessage, bool) {
	var all map[string]json.RawMessage
	if json.Unmarshal(raw, &all) != nil {
		return nil, false
	}
	v, ok := all[name]
	return v, ok
}

// ollamaRequestFields is the set of top-level members the codec translates.
var ollamaRequestFields = map[string]bool{
	"model": true, "messages": true, "stream": true, "think": true,
	"tools": true, "options": true, "keep_alive": true, "format": true,
}

// ollamaFormat maps a chat/responses response_format onto Ollama's format
// field: json_object → "json", json_schema → its bare schema, anything else
// passes through (a same-protocol Ollama request already carries the native
// shape).
func ollamaFormat(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	var typ string
	_ = json.Unmarshal(m["type"], &typ)
	switch typ {
	case "json_object":
		return json.RawMessage(`"json"`)
	case "json_schema":
		if inner, ok := m["json_schema"]; ok {
			var obj map[string]json.RawMessage
			if json.Unmarshal(inner, &obj) == nil {
				return obj["schema"]
			}
		}
		if schema, ok := m["schema"]; ok {
			return schema
		}
		return raw
	default:
		return raw
	}
}

type wireResponse struct {
	Model                 string      `json:"model"`
	CreatedAt             string      `json:"created_at,omitempty"`
	Message               wireMessage `json:"message"`
	Done                  bool        `json:"done"`
	DoneReason            string      `json:"done_reason,omitempty"`
	Error                 string      `json:"error,omitempty"`
	TotalDuration         int64       `json:"total_duration,omitempty"`
	LoadDuration          int64       `json:"load_duration,omitempty"`
	PromptEvalCount       int         `json:"prompt_eval_count,omitempty"`
	PromptEvalCachedCount int         `json:"prompt_eval_cached_count,omitempty"`
	PromptEvalDuration    int64       `json:"prompt_eval_duration,omitempty"`
	EvalCount             int         `json:"eval_count,omitempty"`
	EvalDuration          int64       `json:"eval_duration,omitempty"`
}

// ---- conversions ----

// effortFromBudget maps an IR thinking budget (budget_tokens) onto the
// Ollama effort levels. The mapping is a documented heuristic: Ollama
// levels are qualitative, budget numbers are not; thresholds follow the
// common tiering of extended-thinking budgets.
func effortFromBudget(budget int) string {
	switch {
	case budget >= 8192:
		return "high"
	case budget >= 4096:
		return "medium"
	case budget > 0:
		return "low"
	default:
		return "max"
	}
}

// effortFor picks the think effort for a request: the compat-policy
// mapping (ir.ReasoningEffort, set by routing) wins over the codec's own
// budget heuristic — the policy layer owns the decision so it is
// observable in route_trace, never silent.
func effortFor(req ir.Request) string {
	if req.ReasoningEffort != "" {
		return req.ReasoningEffort
	}
	return effortFromBudget(req.Thinking.BudgetTokens)
}

func stopReasonFromWire(s string) ir.StopReason {
	switch s {
	case "stop":
		return ir.StopEndTurn
	case "tool_calls":
		return ir.StopToolUse
	case "length":
		return ir.StopMaxTokens
	default:
		// done_reason value strings are only loosely pinned; unknown values
		// decode to the neutral natural-completion reason.
		return ir.StopEndTurn
	}
}

func stopReasonToWire(s ir.StopReason) string {
	switch s {
	case ir.StopEndTurn:
		return "stop"
	case ir.StopToolUse:
		return "tool_calls"
	case ir.StopMaxTokens:
		return "length"
	default:
		return "stop"
	}
}

func usageFromWire(w wireResponse) ir.Usage {
	return ir.Usage{
		InputTokens:          w.PromptEvalCount,
		OutputTokens:         w.EvalCount,
		CacheReadInputTokens: w.PromptEvalCachedCount,
	}
}

// synthesizeID derives a stable non-empty response ID from model+created_at
// — Ollama responses carry no ID field.
func synthesizeID(model, createdAt string) string {
	sum := sha256.Sum256([]byte(model + "\x00" + createdAt))
	return "ollama-" + hex.EncodeToString(sum[:8])
}

func boolPtr(b bool) *bool    { return &b }
func strPtr(s string) *string { return &s }

// NewStreamDecoder returns the Ollama NDJSON stream decoder for r.
func NewStreamDecoder(r io.Reader) translate.StreamDecoder {
	return newStreamDecoder(r)
}
