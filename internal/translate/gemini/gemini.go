// Package gemini implements the Google Gemini v1beta REST codec pair
// (generativelanguage.googleapis.com, POST /v1beta/models/{model}:generateContent
// and :streamGenerateContent?alt=sse) against the canonical IR. Wire shapes
// derive from the golden fixtures in testdata/golden/gemini/ (official API
// reference + the v1beta discovery doc, accessed 2026-11-03).
package gemini

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/tubeof/proxyllm/internal/translate"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// SourceProtocol is the ir.Request.SourceProtocol value for a Gemini-decoded
// request (same-protocol passthrough key).
const SourceProtocol = "gemini"

// Codec decodes and encodes Gemini v1beta generateContent wire bodies.
type Codec struct{}

// DecodeRequest decodes a Gemini GenerateContentRequest into the IR.
// contents[] decode role "user"→RoleUser / "model"→RoleAssistant /
// "function"→RoleTool; systemInstruction feeds System; tools are
// declaration-shaped; the toolConfig functionCallingConfig mode maps onto the
// IR ToolChoice.
func (Codec) DecodeRequest(raw []byte) (ir.Request, error) {
	var w wireRequest
	if err := json.Unmarshal(raw, &w); err != nil {
		return ir.Request{}, fmt.Errorf("gemini: decode request: %w", err)
	}
	req := ir.Request{
		Model:          w.Model,
		SourceProtocol: SourceProtocol,
		Passthrough:    ir.UnknownFields(raw, geminiRequestFields),
	}
	if w.GenerationConfig != nil {
		w.GenerationConfig.intoIR(&req)
		// Nested generationConfig unknowns ride a synthetic passthrough key:
		// the IR has no nested-extras channel, and a same-protocol re-encode
		// re-merges them into generationConfig (the key itself is never a
		// wire field).
		if ex := w.GenerationConfig.extrasJSON(); len(ex) > 0 {
			if req.Passthrough == nil {
				req.Passthrough = map[string]json.RawMessage{}
			}
			req.Passthrough[generationConfigExtrasKey] = ex
		}
	}
	if w.SystemInstruction != nil {
		for _, p := range partsFromWire(w.SystemInstruction.Parts) {
			if tp, ok := p.(ir.TextPart); ok {
				req.System = append(req.System, tp)
			}
			// systemInstruction is "Currently, text only" per the reference;
			// a non-text part has no system slot and is documented as dropped.
		}
	}
	for _, wc := range w.Contents {
		switch wc.Role {
		case "", "user":
			req.Messages = append(req.Messages,
				ir.Message{Role: ir.RoleUser, Parts: partsFromWire(wc.Parts)})
		case "model":
			req.Messages = append(req.Messages,
				ir.Message{Role: ir.RoleAssistant, Parts: partsFromWire(wc.Parts)})
		case "function":
			// The current Tool reference describes the function-response turn
			// with Content.role "function"; decode it like a tool-result turn.
			req.Messages = append(req.Messages,
				ir.Message{Role: ir.RoleTool, Parts: partsFromWire(wc.Parts)})
		default:
			return ir.Request{}, fmt.Errorf("gemini: unsupported content role %q (user, model or function)", wc.Role)
		}
	}
	for _, wt := range w.Tools {
		for _, fd := range wt.FunctionDeclarations {
			if fd.Name == "" {
				continue
			}
			req.Tools = append(req.Tools, ir.Tool{
				Name:        fd.Name,
				Description: fd.Description,
				InputSchema: fd.Parameters,
			})
		}
	}
	if w.ToolConfig != nil && w.ToolConfig.FunctionCallingConfig != nil {
		req.ToolChoice = toolChoiceFromWire(w.ToolConfig.FunctionCallingConfig)
	}
	return req, nil
}

// EncodeRequest encodes an IR request into a GenerateContentRequest body.
// The model name rides the URL path, never the body. Thinking maps onto
// generationConfig.thinkingConfig: includeThoughts when the IR enabled or
// adapts thinking, thinkingLevel from the compat-policy effort mapping
// (ReasoningEffort wins — routing owns that decision, never the codec).
func (Codec) EncodeRequest(req ir.Request) ([]byte, error) {
	w := wireRequest{}
	if len(req.System) > 0 {
		w.SystemInstruction = &wireContent{Role: "user"}
		for _, tp := range req.System {
			w.SystemInstruction.Parts = append(w.SystemInstruction.Parts, wirePart{Text: tp.Text})
		}
	}
	// lastCalls remembers the latest assistant turn's tool calls so the
	// following function responses can name the function they answer: the
	// wire requires functionResponse.name, while the IR result part carries
	// only the id.
	var lastCalls []ir.ToolCallPart
	for _, m := range req.Messages {
		switch m.Role {
		case ir.RoleAssistant:
			lastCalls = lastCalls[:0]
			for _, p := range m.Parts {
				if tc, ok := p.(ir.ToolCallPart); ok {
					lastCalls = append(lastCalls, tc)
				}
			}
			wc := wireContent{Role: "model", Parts: contentPartsToWire(m.Parts)}
			if len(wc.Parts) == 0 {
				wc.Parts = []wirePart{}
			}
			w.Contents = append(w.Contents, wc)
		case ir.RoleUser:
			w.Contents = append(w.Contents, wireContent{
				Role:  "user",
				Parts: messagePartsToWire(m.Parts, lastCalls),
			})
		case ir.RoleTool:
			w.Contents = append(w.Contents, wireContent{
				Role:  "user",
				Parts: messagePartsToWire(m.Parts, lastCalls),
			})
		case ir.RoleSystem:
			// A mid-conversation system message has no Gemini role; its text
			// rides a user turn (documented degradation in the matrix).
			parts := []wirePart{}
			for _, p := range m.Parts {
				if tp, ok := p.(ir.TextPart); ok {
					parts = append(parts, wirePart{Text: tp.Text})
				}
			}
			w.Contents = append(w.Contents, wireContent{Role: "user", Parts: parts})
		}
	}
	for _, t := range req.Tools {
		schema := t.InputSchema
		if len(schema) == 0 || string(schema) == "null" {
			// Matrix row: an absent schema is always encoded as an empty
			// object — an omitted parameters field can be rejected upstream.
			schema = json.RawMessage("{}")
		}
		w.Tools = append(w.Tools, wireTool{FunctionDeclarations: []wireFunctionDeclaration{{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  schema,
		}}})
	}
	if tc := toolConfigFromIR(req.ToolChoice); tc != nil {
		w.ToolConfig = tc
	}
	if gc := generationConfigFromIR(req); gc != nil {
		w.GenerationConfig = gc
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("gemini: encode request: %w", err)
	}
	extra := ir.PassthroughFor(req, SourceProtocol)
	raw, extra, err = applyGenerationExtras(raw, extra)
	if err != nil {
		return nil, fmt.Errorf("gemini: encode request: %w", err)
	}
	return ir.MergePassthrough(raw, extra)
}

// generationConfigExtrasKey is the synthetic passthrough key carrying nested
// generationConfig unknowns (the IR has no nested-extras channel). Never a
// wire field.
const generationConfigExtrasKey = "_geminiGenerationConfigExtras"

// extrasJSON renders the captured unknown generationConfig members, or nil.
func (g *wireGenerationConfig) extrasJSON() json.RawMessage {
	if len(g.Extra) == 0 {
		return nil
	}
	return mustJSON(g.Extra)
}

// applyGenerationExtras re-merges the synthetic extras key back into the
// encoded generationConfig (typed members win) and removes the key from the
// passthrough so it never reaches the wire as a top-level field. The
// passthrough map is copied, never mutated — the request may be re-encoded
// for another provider.
func applyGenerationExtras(raw []byte, extra map[string]json.RawMessage) ([]byte, map[string]json.RawMessage, error) {
	ex, ok := extra[generationConfigExtrasKey]
	if !ok {
		return raw, extra, nil
	}
	out := make(map[string]json.RawMessage, len(extra))
	for k, v := range extra {
		if k != generationConfigExtrasKey {
			out[k] = v
		}
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, out, err
	}
	var gc map[string]json.RawMessage
	if g := top["generationConfig"]; len(g) > 0 {
		_ = json.Unmarshal(g, &gc)
	}
	if gc == nil {
		gc = map[string]json.RawMessage{}
	}
	var exMap map[string]json.RawMessage
	if json.Unmarshal(ex, &exMap) == nil {
		for k, v := range exMap {
			if _, exists := gc[k]; !exists {
				gc[k] = v
			}
		}
	}
	g, err := json.Marshal(gc)
	if err != nil {
		return nil, out, err
	}
	top["generationConfig"] = g
	merged, err := json.Marshal(top)
	if err != nil {
		return nil, out, err
	}
	return merged, out, nil
}

// DecodeResponse decodes a (complete) GenerateContentResponse into the IR.
// A promptFeedback.blockReason arrives with NO candidates and normalizes to a
// content-filter stop; a STOP finish whose parts carry function calls
// normalizes to a tool-use stop (the wire has no tool-calls finish reason —
// only the first candidate is taken, candidateCount > 1 is a documented
// degradation).
func (Codec) DecodeResponse(raw []byte) (ir.Response, error) {
	var w wireResponse
	if err := json.Unmarshal(raw, &w); err != nil {
		return ir.Response{}, fmt.Errorf("gemini: decode response: %w", err)
	}
	if w.Error != nil {
		msg := w.Error.Message
		if msg == "" {
			msg = w.Error.Status
		}
		return ir.Response{}, fmt.Errorf("gemini: upstream error: %s", msg)
	}
	resp := ir.Response{
		ID:    w.ResponseID,
		Model: w.ModelVersion,
		Usage: usageFromWire(w.UsageMetadata),
	}
	if w.ResponseID == "" {
		resp.ID = synthesizeID(w.ModelVersion, w.UsageMetadata)
	}
	if len(w.Candidates) == 0 {
		// "Returns no candidates at all only if there was something wrong with
		// the prompt (check prompt_feedback)" — the blockReason IS the stop.
		if w.PromptFeedback != nil && w.PromptFeedback.BlockReason != "" {
			resp.StopReason = ir.StopContentFilter
		}
		return resp, nil
	}
	resp.StopReason = stopReasonFromWire(w.Candidates[0].FinishReason)
	resp.Content = append(resp.Content, partsFromWire(w.Candidates[0].Parts())...)
	// The wire stops a tool-call turn with STOP; the IR names it honestly.
	if resp.StopReason == ir.StopEndTurn && hasToolCall(resp.Content) {
		resp.StopReason = ir.StopToolUse
	}
	return resp, nil
}

// EncodeResponse encodes an IR response into a GenerateContentResponse body.
// The IR tool-use stop encodes as STOP (the wire's natural stop carries the
// tool-call signal via the functionCall part).
func (Codec) EncodeResponse(resp ir.Response) ([]byte, error) {
	cand := wireCandidate{
		Content:      &wireContent{Role: "model", Parts: contentPartsToWire(resp.Content)},
		FinishReason: stopReasonToWire(resp.StopReason),
	}
	if cand.FinishReason == "" {
		cand.FinishReason = finishReasonStop
	}
	w := wireResponse{
		ModelVersion:  resp.Model,
		ResponseID:    resp.ID,
		UsageMetadata: usageMetadataFromIR(resp.Usage),
	}
	w.Candidates = []wireCandidate{cand}
	raw, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("gemini: encode response: %w", err)
	}
	return raw, nil
}

// EncodeCountTokensRequest wraps an encoded generateContent body as a
// countTokens body. The reference is explicit: contents[] and
// generateContentRequest are mutually exclusive — the provider sends the full
// request (system, tools included) and therefore never adds contents[].
func EncodeCountTokensRequest(generateContentBody []byte) ([]byte, error) {
	body, err := json.Marshal(wireCountTokensRequest{
		GenerateContentRequest: json.RawMessage(generateContentBody),
	})
	if err != nil {
		return nil, fmt.Errorf("gemini: encode countTokens request: %w", err)
	}
	return body, nil
}

// DecodeCountTokensResponse extracts totalTokens from a countTokens reply.
func DecodeCountTokensResponse(raw []byte) (int, error) {
	var w struct {
		TotalTokens int `json:"totalTokens"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return 0, fmt.Errorf("gemini: decode countTokens response: %w", err)
	}
	return w.TotalTokens, nil
}

// ---- wire types (mirror the golden fixtures) ----

// wireRequest mirrors the GenerateContentRequest schema. Model rides the URL
// path; the body's optional model field is read on decode only.
type wireRequest struct {
	Contents          []wireContent         `json:"contents,omitempty"`
	SystemInstruction *wireContent          `json:"systemInstruction,omitempty"`
	Tools             []wireTool            `json:"tools,omitempty"`
	ToolConfig        *wireToolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *wireGenerationConfig `json:"generationConfig,omitempty"`
	Model             string                `json:"-"`
}

func (w *wireRequest) UnmarshalJSON(b []byte) error {
	type alias wireRequest
	aux := struct {
		*alias
		Model string `json:"model"`
	}{alias: (*alias)(w)}
	return json.Unmarshal(b, &aux)
}

// geminiRequestFields is the set of top-level members the codec translates.
// Everything else (safetySettings, labels, cachedContent, store, ...) is
// captured as same-protocol passthrough.
var geminiRequestFields = map[string]bool{
	"contents": true, "systemInstruction": true, "tools": true,
	"toolConfig": true, "generationConfig": true, "model": true,
}

// wireGenerationConfig carries the typed members plus a verbatim capture of
// unknown ones (the "nested unknowns live in the codec wire structs' extras"
// pattern) so a same-protocol re-encode keeps them.
type wireGenerationConfig struct {
	MaxOutputTokens  int                 `json:"maxOutputTokens,omitempty"`
	Temperature      *float64            `json:"temperature,omitempty"`
	TopP             *float64            `json:"topP,omitempty"`
	TopK             *int                `json:"topK,omitempty"`
	StopSequences    []string            `json:"stopSequences,omitempty"`
	ResponseFormat   *wireResponseFormat `json:"responseFormat,omitempty"`
	ResponseMimeType string              `json:"responseMimeType,omitempty"`
	ResponseSchema   json.RawMessage     `json:"responseSchema,omitempty"`
	ThinkingConfig   *wireThinkingConfig `json:"thinkingConfig,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

func (g *wireGenerationConfig) UnmarshalJSON(b []byte) error {
	type typed wireGenerationConfig
	var t typed
	if err := json.Unmarshal(b, &t); err != nil {
		return err
	}
	*g = wireGenerationConfig(t)
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	for k, v := range m {
		if genConfigKnown[k] {
			continue
		}
		if g.Extra == nil {
			g.Extra = map[string]json.RawMessage{}
		}
		g.Extra[k] = v
	}
	return nil
}

func (g wireGenerationConfig) MarshalJSON() ([]byte, error) {
	type typed wireGenerationConfig
	raw, err := json.Marshal(typed(g))
	if err != nil {
		return nil, err
	}
	if len(g.Extra) == 0 {
		return raw, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	for k, v := range g.Extra {
		if _, exists := m[k]; !exists {
			m[k] = v
		}
	}
	return json.Marshal(m)
}

var genConfigKnown = map[string]bool{
	"maxOutputTokens": true, "temperature": true, "topP": true, "topK": true,
	"stopSequences": true, "responseFormat": true, "responseMimeType": true,
	"responseSchema": true, "thinkingConfig": true,
}

type wireResponseFormat struct {
	Text *wireTextResponseFormat `json:"text,omitempty"`
}

type wireTextResponseFormat struct {
	MimeType string          `json:"mimeType,omitempty"`
	Schema   json.RawMessage `json:"schema,omitempty"`
}

type wireThinkingConfig struct {
	IncludeThoughts bool   `json:"includeThoughts,omitempty"`
	ThinkingBudget  int    `json:"thinkingBudget,omitempty"`
	ThinkingLevel   string `json:"thinkingLevel,omitempty"`
}

// ThinkingLevel values (ThinkingConfig schema enum, accessed 2026-11-03).
var thinkingLevels = map[string]string{
	"minimal": "MINIMAL",
	"low":     "LOW",
	"medium":  "MEDIUM",
	"high":    "HIGH",
	"max":     "HIGH",
}

type wireResponse struct {
	Candidates     []wireCandidate     `json:"candidates,omitempty"`
	PromptFeedback *wirePromptFeedback `json:"promptFeedback,omitempty"`
	ModelVersion   string              `json:"modelVersion,omitempty"`
	ResponseID     string              `json:"responseId,omitempty"`
	UsageMetadata  *wireUsageMetadata  `json:"usageMetadata,omitempty"`
	Error          *wireResponseError  `json:"error,omitempty"`
}

type wireResponseError struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Status  string `json:"status,omitempty"`
}

type wireCandidate struct {
	Content      *wireContent `json:"content,omitempty"`
	FinishReason string       `json:"finishReason,omitempty"`
}

// Parts returns the candidate's parts (a nil-content candidate has none).
func (c wireCandidate) Parts() []wirePart {
	if c.Content == nil {
		return nil
	}
	return c.Content.Parts
}

// wirePromptFeedback blockReason values: BLOCK_REASON_UNSPECIFIED, SAFETY,
// OTHER, BLOCKLIST, PROHIBITED_CONTENT, IMAGE_SAFETY (accessed 2026-11-03).
type wirePromptFeedback struct {
	BlockReason string `json:"blockReason,omitempty"`
}

type wireUsageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount    int `json:"candidatesTokenCount,omitempty"`
	TotalTokenCount         int `json:"totalTokenCount,omitempty"`
	CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount,omitempty"`
}

// FinishReason values (Candidate schema enum, accessed 2026-11-03) — the
// codec's mapping lives in stopReasonFromWire/stopReasonToWire.
const (
	finishReasonStop           = "STOP"
	finishReasonMaxTokens      = "MAX_TOKENS"
	finishReasonSafety         = "SAFETY"
	finishReasonContinuation   = "CONTINUATION"
	finishReasonUnspecified    = "FINISH_REASON_UNSPECIFIED"
	finishReasonMissingThought = "MISSING_THOUGHT_SIGNATURE"
)

type wireContent struct {
	Role  string     `json:"role,omitempty"`
	Parts []wirePart `json:"parts,omitempty"`
}

type wirePart struct {
	Text             string                `json:"text,omitempty"`
	Thought          bool                  `json:"thought,omitempty"`
	ThoughtSignature string                `json:"thoughtSignature,omitempty"`
	InlineData       *wireBlob             `json:"inlineData,omitempty"`
	FileData         *wireFileData         `json:"fileData,omitempty"`
	FunctionCall     *wireFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *wireFunctionResponse `json:"functionResponse,omitempty"`
}

type wireBlob struct {
	MimeType    string `json:"mimeType,omitempty"`
	Data        string `json:"data,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
}

type wireFileData struct {
	MimeType    string `json:"mimeType,omitempty"`
	FileUri     string `json:"fileUri,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
}

type wireFunctionCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name,omitempty"`
	Args json.RawMessage `json:"args,omitempty"`
}

type wireFunctionResponse struct {
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
}

type wireTool struct {
	FunctionDeclarations []wireFunctionDeclaration `json:"functionDeclarations,omitempty"`
}

type wireFunctionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type wireToolConfig struct {
	FunctionCallingConfig *wireFunctionCallingConfig `json:"functionCallingConfig,omitempty"`
}

// FunctionCallingConfig Mode values: MODE_UNSPECIFIED, AUTO, ANY, NONE,
// VALIDATED; allowedFunctionNames is only legal with ANY / VALIDATED
// (accessed 2026-11-03).
type wireFunctionCallingConfig struct {
	Mode                 string   `json:"mode,omitempty"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

// wireCountTokensRequest: contents[] and generateContentRequest are mutually
// exclusive; this codec always sends the full request.
type wireCountTokensRequest struct {
	GenerateContentRequest json.RawMessage `json:"generateContentRequest,omitempty"`
}

// ---- part conversions ----

// partsFromWire maps one Content's wire parts onto IR parts. The
// thoughtSignature rides verbatim: a thought part carries it inside its
// ThinkingPart, a signature on any other part becomes an empty-thinking
// ThinkingPart directly after that part (encode re-attaches it).
func partsFromWire(parts []wirePart) []ir.Part {
	if len(parts) == 0 {
		return nil
	}
	out := make([]ir.Part, 0, len(parts))
	attachSig := func(base ir.Part, sig string, p wirePart) {
		out = append(out, base)
		if sig != "" && p.InlineData == nil && p.FileData == nil {
			out = append(out, ir.ThinkingPart{Signature: sig})
		}
	}
	for _, p := range parts {
		switch {
		case p.Thought:
			out = append(out, ir.ThinkingPart{Thinking: p.Text, Signature: p.ThoughtSignature})
		case p.FunctionCall != nil:
			args := p.FunctionCall.Args
			if len(args) == 0 || string(args) == "null" {
				args = json.RawMessage("{}")
			}
			attachSig(ir.ToolCallPart{
				ID:    p.FunctionCall.ID,
				Name:  p.FunctionCall.Name,
				Input: args,
			}, p.ThoughtSignature, p)
		case p.FunctionResponse != nil:
			raw := p.FunctionResponse.Response
			if len(raw) == 0 || string(raw) == "null" {
				raw = json.RawMessage("{}")
			}
			out = append(out, ir.ToolResultPart{
				ToolUseID: p.FunctionResponse.ID,
				Content:   []ir.Part{ir.TextPart{Text: string(raw)}},
			})
		case p.InlineData != nil:
			out = append(out, ir.ImagePart{
				Source:    "base64",
				MediaType: p.InlineData.MimeType,
				Data:      p.InlineData.Data,
			})
			// A signature on an inline media part is dropped: the IR image
			// part has no signature channel (documented degradation).
		case p.FileData != nil:
			out = append(out, ir.ImagePart{
				Source:    "url",
				URL:       p.FileData.FileUri,
				MediaType: p.FileData.MimeType,
			})
		case p.Text != "":
			attachSig(ir.TextPart{Text: p.Text}, p.ThoughtSignature, p)
		case p.ThoughtSignature != "":
			// A bare signature part (no other payload) — signature verbatim.
			out = append(out, ir.ThinkingPart{Signature: p.ThoughtSignature})
		default:
			// Unknown part kinds (server-side toolCall/toolResponse,
			// executableCode, ...) have no IR channel; dropped, documented in
			// the translation matrix.
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// contentPartsToWire maps IR response/assistant parts onto wire parts. An
// empty-thinking ThinkingPart carrying a signature is re-attached to the
// part it decorated (decode appends it right after the owner); a signature
// that found no owner rides a bare signature part so the value stays
// verbatim — thinking signatures are never rewritten.
func contentPartsToWire(parts []ir.Part) []wirePart {
	out := make([]wirePart, 0, len(parts))
	for _, p := range parts {
		switch pt := p.(type) {
		case ir.TextPart:
			out = append(out, wirePart{Text: pt.Text})
		case ir.ImagePart:
			switch pt.Source {
			case "base64":
				out = append(out, wirePart{InlineData: &wireBlob{
					MimeType: pt.MediaType, Data: pt.Data,
				}})
			case "url":
				out = append(out, wirePart{FileData: &wireFileData{
					MimeType: pt.MediaType, FileUri: pt.URL,
				}})
			}
			// "file" sources name another account's uploaded file; cross-
			// protocol they drop (same rule as every other wire).
		case ir.ToolCallPart:
			args := pt.Input
			if len(args) == 0 || string(args) == "null" {
				args = json.RawMessage("{}")
			}
			out = append(out, wirePart{FunctionCall: &wireFunctionCall{
				ID:   pt.ID,
				Name: pt.Name,
				Args: args,
			}})
		case ir.ThinkingPart:
			if pt.Thinking != "" {
				out = append(out, wirePart{
					Text:             pt.Thinking,
					Thought:          true,
					ThoughtSignature: pt.Signature,
				})
				continue
			}
			attachSignature(&out, pt.Signature)
		default:
			// Tool results never appear in model turns; server-tool parts
			// have no Gemini chat channel.
		}
	}
	return out
}

// attachSignature puts a bare signature onto the previous wire part, or
// emits a standalone signature part when nothing precedes it.
func attachSignature(out *[]wirePart, sig string) {
	if sig == "" {
		return
	}
	if n := len(*out); n > 0 && (*out)[n-1].ThoughtSignature == "" {
		(*out)[n-1].ThoughtSignature = sig
		return
	}
	*out = append(*out, wirePart{ThoughtSignature: sig})
}

// messagePartsToWire encodes the parts of a user/tool-result turn. Tool
// results become functionResponse parts; the function NAME comes from the
// matching earlier tool call (by id, else the latest call).
func messagePartsToWire(parts []ir.Part, calls []ir.ToolCallPart) []wirePart {
	out := make([]wirePart, 0, len(parts))
	for _, p := range parts {
		if tr, ok := p.(ir.ToolResultPart); ok {
			out = append(out, functionResponsePart(tr, calls))
			continue
		}
		out = append(out, contentPartsToWire([]ir.Part{p})...)
	}
	return out
}

// functionResponsePart renders one tool result as a functionResponse part.
// The response object is the stored raw JSON when it is an object (verbatim
// same-protocol round trip); a plain string becomes {"output": …}; an error
// result wraps its detail in {"error": …}.
func functionResponsePart(tr ir.ToolResultPart, calls []ir.ToolCallPart) wirePart {
	name := ""
	for _, c := range calls {
		if tr.ToolUseID != "" && c.ID == tr.ToolUseID {
			name = c.Name
			break
		}
	}
	if name == "" && len(calls) == 1 && calls[0].Name != "" {
		// Parallel calls disambiguate by id; a single prior call needs no id.
		name = calls[0].Name
	}
	text := ""
	for _, cp := range tr.Content {
		if tp, ok := cp.(ir.TextPart); ok {
			text += tp.Text
		}
	}
	var response json.RawMessage
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &obj); err == nil && obj != nil {
		response = json.RawMessage(text)
		if tr.IsError {
			// Keep the decoded object but flag it: the reference's documented
			// error channel in a function response.
			if _, hasError := obj["error"]; !hasError {
				wrapped, merr := json.Marshal(map[string]json.RawMessage{"error": response})
				if merr == nil {
					response = wrapped
				}
			}
		}
	} else if tr.IsError {
		response = mustJSON(map[string]string{"error": text})
	} else {
		response = mustJSON(map[string]string{"output": text})
	}
	return wirePart{FunctionResponse: &wireFunctionResponse{
		ID:       tr.ToolUseID,
		Name:     name,
		Response: response,
	}}
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return raw
}

// ---- generationConfig conversions ----

// intoIR applies the generationConfig members onto the IR request.
func (g *wireGenerationConfig) intoIR(req *ir.Request) {
	req.MaxTokens = g.MaxOutputTokens
	if g.Temperature != nil {
		t := *g.Temperature
		req.Temperature = &t
	}
	if g.TopP != nil {
		t := *g.TopP
		req.TopP = &t
	}
	if g.TopK != nil {
		k := *g.TopK
		req.TopK = &k
	}
	if len(g.StopSequences) > 0 {
		req.StopSequences = g.StopSequences
	}
	if g.ThinkingConfig != nil {
		if g.ThinkingConfig.IncludeThoughts || g.ThinkingConfig.ThinkingBudget > 0 {
			req.Thinking = &ir.ThinkingConfig{
				Enabled:      true,
				BudgetTokens: g.ThinkingConfig.ThinkingBudget,
			}
		}
		if lvl := g.ThinkingConfig.ThinkingLevel; lvl != "" {
			if s := strings.ToLower(lvl); s == "minimal" || s == "low" || s == "medium" || s == "high" {
				req.ReasoningEffort = s
			}
		}
	}
	if rf := responseFormatFromWire(g); len(rf) > 0 {
		req.ResponseFormat = rf
	}
}

// responseFormatFromWire normalizes the wire's output-format config (the
// modern responseFormat.text or the legacy responseMimeType/responseSchema
// duo) into the IR's chat response_format shape.
func responseFormatFromWire(g *wireGenerationConfig) json.RawMessage {
	mime := g.ResponseMimeType
	schema := g.ResponseSchema
	if g.ResponseFormat != nil && g.ResponseFormat.Text != nil {
		if mt := g.ResponseFormat.Text.MimeType; mt == "APPLICATION_JSON" {
			mime = "application/json"
			if len(g.ResponseFormat.Text.Schema) > 0 && string(g.ResponseFormat.Text.Schema) != "null" {
				schema = g.ResponseFormat.Text.Schema
			}
		}
	}
	if mime != "application/json" {
		return nil
	}
	if len(schema) > 0 && string(schema) != "null" {
		return mustJSON(map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"schema": schema},
		})
	}
	return json.RawMessage(`{"type":"json_object"}`)
}

// generationConfigFromIR renders the IR request knobs the wire understands.
// nil when nothing is set — an empty generationConfig object is noise.
func generationConfigFromIR(req ir.Request) *wireGenerationConfig {
	g := &wireGenerationConfig{}
	if req.MaxTokens > 0 {
		g.MaxOutputTokens = req.MaxTokens
	}
	g.Temperature = req.Temperature
	g.TopP = req.TopP
	g.TopK = req.TopK
	if len(req.StopSequences) > 0 {
		g.StopSequences = req.StopSequences
	}
	if rf := responseFormatForIR(req.ResponseFormat); rf != nil {
		g.ResponseFormat = rf
	}
	tc := &wireThinkingConfig{}
	if req.Thinking != nil && (req.Thinking.Enabled || req.Thinking.Adaptive) {
		tc.IncludeThoughts = true
		if req.Thinking.Enabled && !req.Thinking.Adaptive && req.Thinking.BudgetTokens > 0 {
			tc.ThinkingBudget = req.Thinking.BudgetTokens
		}
	}
	if lvl := thinkingLevelFor(req); lvl != "" {
		tc.ThinkingLevel = lvl
	}
	if tc.IncludeThoughts || tc.ThinkingBudget > 0 || tc.ThinkingLevel != "" {
		g.ThinkingConfig = tc
	}
	if g.MaxOutputTokens == 0 && g.Temperature == nil && g.TopP == nil && g.TopK == nil &&
		len(g.StopSequences) == 0 && g.ResponseFormat == nil && g.ThinkingConfig == nil {
		return nil
	}
	return g
}

// thinkingLevelFor resolves the wire thinkingLevel: the compat-policy effort
// mapping wins over the codec's budget heuristic (see ir.EffortFromBudget's
// tiering).
func thinkingLevelFor(req ir.Request) string {
	effort := req.ReasoningEffort
	if effort == "" && req.Thinking != nil && req.Thinking.Enabled {
		effort = ir.EffortFromBudget(req.Thinking.BudgetTokens)
	}
	return thinkingLevels[effort]
}

// responseFormatForIR maps the IR chat response_format onto the wire's
// responseFormat.text config.
func responseFormatForIR(raw json.RawMessage) *wireResponseFormat {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	var typ string
	_ = json.Unmarshal(m["type"], &typ)
	switch typ {
	case "json_object":
		return &wireResponseFormat{Text: &wireTextResponseFormat{MimeType: "APPLICATION_JSON"}}
	case "json_schema":
		schema := m["schema"]
		if inner, ok := m["json_schema"]; ok {
			var obj map[string]json.RawMessage
			if json.Unmarshal(inner, &obj) == nil && obj["schema"] != nil {
				schema = obj["schema"]
			}
		}
		if len(schema) == 0 {
			return &wireResponseFormat{Text: &wireTextResponseFormat{MimeType: "APPLICATION_JSON"}}
		}
		return &wireResponseFormat{Text: &wireTextResponseFormat{
			MimeType: "APPLICATION_JSON", Schema: schema,
		}}
	default:
		return nil
	}
}

// ---- tools / toolConfig conversions ----

// toolChoiceFromWire maps the functionCallingConfig onto the IR. VALIDATED is
// ANY with constrained decoding; allowedFunctionNames has no IR channel and
// drops (documented degradation).
func toolChoiceFromWire(cfg *wireFunctionCallingConfig) *ir.ToolChoice {
	switch cfg.Mode {
	case "NONE":
		return &ir.ToolChoice{Type: ir.ToolChoiceNone}
	case "ANY", "VALIDATED":
		return &ir.ToolChoice{Type: ir.ToolChoiceAny}
	case "AUTO":
		return &ir.ToolChoice{Type: ir.ToolChoiceAuto}
	case "MODE_UNSPECIFIED":
		return nil
	default:
		return nil
	}
}

// toolConfigFromIR renders the IR tool choice as the wire's toolConfig.
func toolConfigFromIR(tc *ir.ToolChoice) *wireToolConfig {
	if tc == nil {
		return nil
	}
	switch tc.Type {
	case ir.ToolChoiceNone:
		return &wireToolConfig{FunctionCallingConfig: &wireFunctionCallingConfig{Mode: "NONE"}}
	case ir.ToolChoiceAny:
		return &wireToolConfig{FunctionCallingConfig: &wireFunctionCallingConfig{Mode: "ANY"}}
	case ir.ToolChoiceTool:
		return &wireToolConfig{FunctionCallingConfig: &wireFunctionCallingConfig{
			Mode:                 "ANY",
			AllowedFunctionNames: []string{tc.Name},
		}}
	default: // auto
		return &wireToolConfig{FunctionCallingConfig: &wireFunctionCallingConfig{Mode: "AUTO"}}
	}
}

// ---- stop reasons ----

// stopReasonFromWire maps the Candidate.finishReason enum onto the IR. Every
// enum value is covered; an UNRECOGNIZED future value decodes to "" (no
// terminal reason), the openai codec's convention for unknown reasons.
//
// Mapping decisions (documented in the translation matrix): the safety-family
// reasons (SAFETY, RECITATION, LANGUAGE, BLOCKLIST, PROHIBITED_CONTENT, SPII,
// the IMAGE_* family, ESCALATION) read as content_filter; the tool-call
// failure reasons (MALFORMED_FUNCTION_CALL, MALFORMED_RESPONSE,
// UNEXPECTED_TOOL_CALL, TOO_MANY_TOOL_CALLS) and MISSING_THOUGHT_SIGNATURE
// read as refusal (a stopped turn no downstream client can treat as a
// completed tool flow); CONTINUATION is the wire's pause-continue mechanic.
func stopReasonFromWire(s string) ir.StopReason {
	switch s {
	case "", finishReasonUnspecified, "OTHER":
		return ""
	case finishReasonStop:
		return ir.StopEndTurn
	case finishReasonMaxTokens:
		return ir.StopMaxTokens
	case finishReasonSafety, "RECITATION", "LANGUAGE", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII",
		"IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_OTHER", "NO_IMAGE", "IMAGE_RECITATION",
		"ESCALATION":
		return ir.StopContentFilter
	case finishReasonMissingThought, "MALFORMED_FUNCTION_CALL", "MALFORMED_RESPONSE",
		"UNEXPECTED_TOOL_CALL", "TOO_MANY_TOOL_CALLS", "PUP_LIMITED_DISABLED":
		return ir.StopRefusal
	case finishReasonContinuation:
		return ir.StopPauseTurn
	default:
		return ""
	}
}

// stopReasonToWire maps an IR stop reason onto the wire. The wire has no
// tool-calls finish reason (the functionCall part carries the signal), no
// stop-sequence distinction (stop sequences report as STOP), and no
// context-window reason.
func stopReasonToWire(s ir.StopReason) string {
	switch s {
	case ir.StopMaxTokens:
		return finishReasonMaxTokens
	case ir.StopContentFilter, ir.StopRefusal:
		return finishReasonSafety
	case ir.StopPauseTurn:
		return finishReasonContinuation
	default:
		return finishReasonStop
	}
}

// ---- usage ----

// usageFromWire maps the token counters; absent fields stay explicit zeros in
// the IR.
func usageFromWire(w *wireUsageMetadata) ir.Usage {
	if w == nil {
		return ir.Usage{}
	}
	return ir.Usage{
		InputTokens:          w.PromptTokenCount,
		OutputTokens:         w.CandidatesTokenCount,
		CacheReadInputTokens: w.CachedContentTokenCount,
		ReasoningTokens:      w.ThoughtsTokenCount,
	}
}

// usageMetadataFromIR renders the wire usage object; totalTokenCount is the
// documented sum of prompt + thoughts + candidates.
func usageMetadataFromIR(u ir.Usage) *wireUsageMetadata {
	return &wireUsageMetadata{
		PromptTokenCount:        u.InputTokens,
		CandidatesTokenCount:    u.OutputTokens,
		TotalTokenCount:         u.InputTokens + u.OutputTokens + u.ReasoningTokens,
		CachedContentTokenCount: u.CacheReadInputTokens,
		ThoughtsTokenCount:      u.ReasoningTokens,
	}
}

// hasToolCall reports whether the decoded content carries a tool call.
func hasToolCall(content []ir.Part) bool {
	for _, p := range content {
		if _, ok := p.(ir.ToolCallPart); ok {
			return true
		}
	}
	return false
}

// synthesizeID derives a stable non-empty response ID — responses normally
// carry responseId; a body without one still gets an ID (the ollama codec's
// rule).
func synthesizeID(model string, usage *wireUsageMetadata) string {
	count := 0
	if usage != nil {
		count = usage.TotalTokenCount
	}
	sum := sha256.Sum256([]byte(model + "\x00" + fmt.Sprint(count)))
	return "gemini-" + hex.EncodeToString(sum[:8])
}

// NewStreamDecoder returns the Gemini alt=sse stream decoder for r.
func NewStreamDecoder(r io.Reader) translate.StreamDecoder {
	return newStreamDecoder(r)
}
