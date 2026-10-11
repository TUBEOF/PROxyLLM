package httpapi

// Request/response capture for the Logs page (2026-10-10). The log has always
// answered "what happened" — status, tokens, provider, route_trace — but not
// "what was actually said". An operator could see that a vision fallback fired
// without being able to read what it answered, or see a 400 without seeing the
// conversation that provoked it.
//
// Two rules shape what is captured:
//
//   - **Display shape, not wire shape.** The capture is rendered in the drawer,
//     never replayed, so it is encoded for reading: parts carry a kind and the
//     fields that matter for that kind, and a tool call's arguments ride as
//     text rather than as nested wire objects.
//   - **No image payloads.** An image part is recorded by its media type, its
//     source and its size — never its bytes. A base64 screenshot is megabytes of
//     noise in a log row, and reading it back adds nothing a size does not.
//
// Everything passes through the redactor before it is stored, the same
// obligation every other log site carries.

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/tubeof/proxyllm/internal/observability"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// captureRedactor masks credential shapes in captured payloads.
var captureRedactor = observability.NewRedactor()

// captureEnabled reports whether the conversation and the answer are stored
// with each log row. Default ON — reading what a request said is the point of
// the capture — with an explicit "0" as the operator's off switch.
//
// Read per request rather than cached, exactly like the other request-time
// settings: a settings write must take effect on the next request, and the
// read is one indexed lookup on a path that is already doing several.
func (a *api) captureEnabled(ctx context.Context) bool {
	if a.store == nil {
		return false
	}
	v, ok, err := a.store.GetConfigValue(ctx, settingKeyCapturePayloads)
	if err != nil || !ok {
		return true
	}
	return strings.TrimSpace(v) != "0"
}

// captureRequest records the conversation on the request's log meta. Only the
// reference is taken here — encoding happens in capturePayload, off the request
// path.
func (a *api) captureRequest(ctx context.Context, req ir.Request) {
	if !a.captureEnabled(ctx) {
		return
	}
	meta := metaFromCtx(ctx)
	if meta == nil {
		return
	}
	reqCopy := req
	meta.capturedReq = &reqCopy
}

// captureResponse records the answer on the request's log meta. Called at the
// response boundary, where the answer is complete. Nil-safe: a request whose
// meta was never installed captures nothing.
func captureResponse(ctx context.Context, resp ir.Response) {
	meta := metaFromCtx(ctx)
	if meta == nil {
		return
	}
	respCopy := resp
	meta.capturedResp = &respCopy
}

// captureResponseText records an answer that arrived as a stream. The relay
// accumulates the text; this is the same contract as captureResponse.
func captureResponseText(ctx context.Context, model string, text string) {
	meta := metaFromCtx(ctx)
	if meta == nil || text == "" {
		return
	}
	respCopy := ir.Response{
		Model:   model,
		Content: []ir.Part{ir.TextPart{Text: text}},
	}
	meta.capturedResp = &respCopy
}

// captureTimeout bounds one payload write; a slow store must not pile goroutines
// up behind the log path.
const captureTimeout = 5 * time.Second

// captureConcurrency caps in-flight payload writes. A sustained flood must not
// grow goroutines without bound (each holds a store connection for up to
// captureTimeout). Excess captures are DROPPED, not queued: the payload is a
// diagnostic beside the audit row, and shedding one under load costs the log
// some readability, never its correctness.
const captureConcurrency = 8

// captureSlots is the bounded semaphore for capturePayload.
var captureSlots = make(chan struct{}, captureConcurrency)

// capturePayload writes the captured conversation and answer beside the row that
// was just inserted.
//
// Off the request path by design: encoding, redaction and the INSERT are real
// work and the client already has its answer. It runs on its own goroutine with
// a bounded context and a bounded in-flight count — the same contract as the
// capability-gap learning: the request never waits for it, and a failure is the
// log's problem, not the client's.
func (a *api) capturePayload(meta *logMeta, rowID int64, ts int64) {
	if meta == nil || rowID <= 0 || (meta.capturedReq == nil && meta.capturedResp == nil) {
		return
	}
	req, resp := meta.capturedReq, meta.capturedResp
	select {
	case captureSlots <- struct{}{}:
	default:
		return // capture backlogged — shed this one (see captureConcurrency)
	}
	go func() {
		defer func() { <-captureSlots }()
		ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
		defer cancel()
		if !a.captureEnabled(ctx) {
			return
		}
		p := storage.RequestPayload{
			OwnerKind: storage.PayloadOwnerRequest,
			OwnerID:   rowID,
			TS:        ts,
		}
		if req != nil {
			p.ClientRequest = capturedRequest(*req)
		}
		if resp != nil {
			p.ClientResponse = capturedResponse(*resp)
		}
		// A failed capture write is a server defect, not a client error — the
		// posture the log row itself already takes.
		_ = a.store.PutRequestPayload(ctx, p)
	}()
}

// capturedMessage is one conversation turn in the display shape.
type capturedMessage struct {
	Role  string         `json:"role"`
	Parts []capturedPart `json:"parts"`
}

// capturedPart is one content part in the display shape. Which fields are set
// depends on Kind.
type capturedPart struct {
	Kind string `json:"kind"`

	// Text carries a text part, a tool call's arguments, a tool result's text
	// and a thinking block.
	Text string `json:"text,omitempty"`

	// Name is the tool name (a call) or the tool-use id (a result).
	Name string `json:"name,omitempty"`

	// Image parts: the source form, the media type, the decoded payload size
	// and — for a remote image — the URL. Never the payload itself.
	Source    string `json:"source,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Bytes     int    `json:"bytes,omitempty"`
	URL       string `json:"url,omitempty"`
}

type capturedRequestShape struct {
	Model    string            `json:"model,omitempty"`
	System   []string          `json:"system,omitempty"`
	Messages []capturedMessage `json:"messages"`
	Tools    []string          `json:"tools,omitempty"`
}

type capturedResponseShape struct {
	Model      string         `json:"model,omitempty"`
	StopReason string         `json:"stop_reason,omitempty"`
	Parts      []capturedPart `json:"parts"`
}

// capturedRequest renders a request as the display JSON the drawer reads,
// redacted. An unencodable request yields "" rather than a half-shaped blob:
// the drawer treats an absent payload as "nothing captured".
func capturedRequest(req ir.Request) string {
	shape := capturedRequestShape{Model: req.Model}
	for _, sp := range req.System {
		if sp.Text != "" {
			shape.System = append(shape.System, sp.Text)
		}
	}
	for _, msg := range req.Messages {
		shape.Messages = append(shape.Messages, capturedMessage{
			Role:  string(msg.Role),
			Parts: capturedParts(msg.Parts),
		})
	}
	for _, tool := range req.Tools {
		shape.Tools = append(shape.Tools, tool.Name)
	}
	return marshalRedacted(shape)
}

// capturedResponse renders a response as the display JSON the drawer reads,
// redacted.
func capturedResponse(resp ir.Response) string {
	return marshalRedacted(capturedResponseShape{
		Model:      resp.Model,
		StopReason: string(resp.StopReason),
		Parts:      capturedParts(resp.Content),
	})
}

func marshalRedacted(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return captureRedactor.Redact(string(raw))
}

func capturedParts(parts []ir.Part) []capturedPart {
	out := make([]capturedPart, 0, len(parts))
	for _, p := range parts {
		switch v := p.(type) {
		case ir.TextPart:
			out = append(out, capturedPart{Kind: "text", Text: v.Text})
		case ir.ImagePart:
			out = append(out, capturedPart{
				Kind:      "image",
				Source:    v.Source,
				MediaType: v.MediaType,
				URL:       v.URL,
				Bytes:     decodedImageBytes(v),
			})
		case ir.ToolCallPart:
			out = append(out, capturedPart{Kind: "tool_call", Name: v.Name, Text: string(v.Input)})
		case ir.ToolResultPart:
			out = append(out, capturedPart{
				Kind: "tool_result",
				Name: v.ToolUseID,
				Text: capturedPartText(v.Content),
			})
		case ir.ThinkingPart:
			out = append(out, capturedPart{Kind: "thinking", Text: v.Thinking})
		case ir.ServerToolUsePart:
			out = append(out, capturedPart{Kind: "server_tool_call", Name: v.Name, Text: string(v.Input)})
		case ir.ServerToolResultPart:
			out = append(out, capturedPart{Kind: "server_tool_result", Name: v.ToolName, Text: v.ErrorCode})
		}
	}
	return out
}

// capturedPartText flattens nested result parts to their text — the drawer
// renders a tool result as one block.
func capturedPartText(parts []ir.Part) string {
	var b strings.Builder
	for _, p := range parts {
		switch v := p.(type) {
		case ir.TextPart:
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(v.Text)
		case ir.ImagePart:
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString("[image ")
			b.WriteString(v.MediaType)
			b.WriteString("]")
		}
	}
	return b.String()
}

// decodedImageBytes is the size of the base64 payload's decoded bytes — the
// fact worth keeping about an image that is not going to be stored. The
// arithmetic lives on ir.ImagePart now, so the routing side (which records the
// same fact for a caption payload) cannot drift from this one.
func decodedImageBytes(p ir.ImagePart) int {
	return p.DecodedBytes()
}
