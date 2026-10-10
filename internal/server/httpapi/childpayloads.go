package httpapi

// Sub-operation payloads on the write path (2026-10-10 Logs page).
//
// A sub-operation's detail view shows what THAT step sent and received, so the
// drawer needs a body per child row — the tool call and its result, the
// instruction a describer got and the caption it wrote. The facts are collected
// by routing (routing.ChildPayload) because that is where they exist; this file
// pairs them with the rows that were just persisted and hands them to the store.
//
// Two rules, both inherited from the parent capture:
//
//   - **Redacted before the write.** A caption is model output derived from an
//     image, and an image is very often a screenshot of a terminal — the one
//     place a credential is most likely to be transcribed verbatim. Everything
//     here goes through the same redactor every other log site uses.
//   - **Off the request path.** The client already has its answer; a payload is
//     a diagnostic beside the audit row. Bounded in-flight count, bounded write
//     timeout, dropped rather than queued under load.

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/storage"
)

// childPayloadRow is one child row's rendered payload, keyed by the row id the
// insert allocated.
type childPayloadRow struct {
	id       int64
	request  string
	response string
}

// captionExcerptRunes bounds the caption excerpt shown on the child ROW (the
// full text lives in the payload, one click away). Long enough to recognize
// which image was described, short enough to stay one table line.
const captionExcerptRunes = 120

// attachChildPayloads pairs the trace-derived child rows with the payloads the
// routing layer collected, and upgrades a caption row's detail from its
// character count to an excerpt of what the describer actually wrote — the
// operator reads "what was the answering model told the image showed?" on the
// row, not a number.
//
// Pairing is by kind in collection order: both lists come from the same event
// sequence in the same order, so the nth emulated-tool op takes the nth
// emulated-tool payload. Nothing else about the rows changes.
func attachChildPayloads(ops []storage.ChildLog, payloads []routing.ChildPayload) ([]storage.ChildLog, []childPayloadRow) {
	rows := make([]childPayloadRow, len(ops))
	used := make([]bool, len(payloads))
	for i := range ops {
		j := matchPayload(payloads, used, routing.OpKind(ops[i].Kind), ops[i])
		if j < 0 {
			continue
		}
		used[j] = true
		p := payloads[j]
		req, resp, ok := renderChildPayload(p)
		if !ok {
			continue
		}
		rows[i] = childPayloadRow{request: req, response: resp}
		if routing.OpKind(ops[i].Kind) == routing.OpImageCaption && p.Output != "" {
			ops[i].Detail = childRedactor.Redact(excerptRunes(p.Output, captionExcerptRunes))
		}
	}
	return ops, rows
}

// matchPayload finds the first unused payload of this kind that names the same
// step. Kind alone would do for the common case; the tool/provider tie-break
// keeps two calls in one request (search then fetch) paired with the right row
// even if the trace and the payloads ever diverge in order.
func matchPayload(payloads []routing.ChildPayload, used []bool, kind routing.OpKind, op storage.ChildLog) int {
	for j, p := range payloads {
		if used[j] || p.Kind != kind {
			continue
		}
		if p.Tool != "" && op.ToRef != "" && p.Tool != op.ToRef {
			continue
		}
		if p.ProviderID != "" && op.ProviderID != "" && p.ProviderID != op.ProviderID {
			continue
		}
		return j
	}
	return -1
}

// renderChildPayload renders one payload as the display JSON the drawer shows:
// a projection for reading, never a wire shape to be replayed.
func renderChildPayload(p routing.ChildPayload) (request, response string, ok bool) {
	switch p.Kind {
	case routing.OpEmulatedTool:
		return marshalChildBody(emulatedToolRequest{Tool: p.Tool, Input: jsonValue(p.Input)}),
			marshalChildBody(emulatedToolResponse{Result: jsonValue(p.Output)}), true
	case routing.OpImageCaption:
		return marshalChildBody(captionRequest{Instruction: p.Input,
				Image: captionImage{MediaType: p.MediaType, Bytes: p.ImageBytes}}),
			marshalChildBody(captionResponse{Caption: p.Output}), true
	}
	// A failover names its reason and nothing else; there is no body to show.
	return "", "", false
}

type emulatedToolRequest struct {
	Tool  string `json:"tool"`
	Input any    `json:"input,omitempty"`
}

type emulatedToolResponse struct {
	Result any `json:"result,omitempty"`
}

type captionRequest struct {
	Instruction string       `json:"instruction"`
	Image       captionImage `json:"image"`
}

// captionImage records the described image by type and size — never its bytes,
// the same rule the parent capture follows.
type captionImage struct {
	MediaType string `json:"media_type,omitempty"`
	Bytes     int    `json:"bytes,omitempty"`
}

type captionResponse struct {
	Caption string `json:"caption"`
}

// jsonValue re-embeds a stored JSON payload (a tool call's arguments, the result
// the model was handed) as a structured value so the drawer renders it as JSON
// rather than as an escaped string. Anything that does not parse — the emulator
// error text a failed step carries — passes through as a plain string, which is
// how it reached the model too.
func jsonValue(s string) any {
	if s == "" {
		return nil
	}
	var v any
	if json.Unmarshal([]byte(s), &v) == nil {
		return v
	}
	return s
}

// marshalChildBody renders one side of a payload, redacted. An unencodable body
// yields "" (the drawer reads an absent side as "nothing captured"), never a
// half-shaped blob.
func marshalChildBody(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return captureRedactor.Redact(string(raw))
}

// excerptRunes shortens s to at most limit runes, never splitting one, and marks
// the cut. Runes, not bytes: a caption can be any language.
func excerptRunes(s string, limit int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

// captureChildPayloads persists the rendered child payloads beside the rows they
// belong to, off the request path — the same contract as capturePayload: a
// bounded in-flight count, a bounded write timeout, dropped rather than queued
// under load, and a failed write is the log's problem, never the client's.
func (a *api) captureChildPayloads(rows []childPayloadRow, ts int64) {
	any := false
	for _, r := range rows {
		if r.id > 0 && (r.request != "" || r.response != "") {
			any = true
			break
		}
	}
	if !any {
		return
	}
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
		for _, r := range rows {
			if r.id <= 0 || (r.request == "" && r.response == "") {
				continue
			}
			// Storage caps each side at 64 KB; the redactor already ran.
			_ = a.store.PutRequestPayload(ctx, storage.RequestPayload{
				OwnerKind:      storage.PayloadOwnerChild,
				OwnerID:        r.id,
				TS:             ts,
				ClientRequest:  r.request,
				ClientResponse: r.response,
			})
		}
	}()
}
