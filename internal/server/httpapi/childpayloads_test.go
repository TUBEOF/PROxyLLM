package httpapi

// Sub-operation payloads on the write path (Logs page).
//
// Spec: the operator's report that a fallback/vision sub-row is unreadable —
// its second line is a character count, it cannot be opened, and nothing about
// what the step did survives the request. The pairing and rendering below are
// what turns a collected routing payload into the drawer's body; the collection
// itself is pinned in internal/routing/childpayloads_test.go.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/storage"
)

func TestAttachChildPayloads_EmulatedToolRowsCarryTheirOwnExchange(t *testing.T) {
	ops := []storage.ChildLog{
		{Kind: string(routing.OpEmulatedTool), ToRef: "web_search"},
		{Kind: string(routing.OpEmulatedTool), ToRef: "web_fetch"},
	}
	payloads := []routing.ChildPayload{
		{Kind: routing.OpEmulatedTool, Tool: "web_search", Input: `{"query":"golang proxies"}`,
			Output: `{"query":"golang proxies","results":[{"url":"https://example.com/a"}]}`},
		{Kind: routing.OpEmulatedTool, Tool: "web_fetch", Input: `{"url":"https://example.com/a"}`,
			Output: `{"url":"https://example.com/a","content":"the page"}`},
	}

	rows, bodies := attachChildPayloads(ops, payloads, true)
	if len(bodies) != 2 {
		t.Fatalf("bodies = %d, want one per call", len(bodies))
	}

	// Paired by tool, not merely by position: each row shows its own exchange.
	search, fetch := bodies[0], bodies[1]
	if !strings.Contains(search.request, "golang proxies") || strings.Contains(search.request, "example.com/a") {
		t.Errorf("search request = %q, want the search's own arguments", search.request)
	}
	if !strings.Contains(search.response, "example.com/a") {
		t.Errorf("search response = %q, want the result the model was handed", search.response)
	}
	if !strings.Contains(fetch.request, `"web_fetch"`) && !strings.Contains(fetch.request, "example.com/a") {
		t.Errorf("fetch request = %q, want the fetch's own arguments", fetch.request)
	}
	if !strings.Contains(fetch.response, "the page") {
		t.Errorf("fetch response = %q, want the fetched document", fetch.response)
	}

	// The rendered sides are valid JSON: the drawer pretty-prints them.
	for i, b := range bodies {
		for side, body := range map[string]string{"request": b.request, "response": b.response} {
			var v any
			if err := json.Unmarshal([]byte(body), &v); err != nil {
				t.Errorf("bodies[%d].%s is not JSON (%v): %q", i, side, err, body)
			}
		}
	}
	// A failover-style op keeps its trace detail: nothing here rewrites rows
	// that have no payload of their own.
	if strings.Contains(rows[0].Detail, "character") {
		t.Errorf("emulated-tool row detail was rewritten: %q", rows[0].Detail)
	}
}

// The operator's actual complaint, pinned: the row must say WHAT was described,
// not how many characters it took.
func TestAttachChildPayloads_CaptionRowShowsTheDescriptionNotACount(t *testing.T) {
	const caption = "a terminal window showing a nil-pointer stack trace in nil.go:42"
	ops := []storage.ChildLog{
		{Kind: string(routing.OpImageCaption), Detail: "63 character(s) described"},
	}
	payloads := []routing.ChildPayload{
		{Kind: routing.OpImageCaption, ProviderID: "vision", Model: "vl-1",
			Input: "Describe this image concisely.", MediaType: "image/png", ImageBytes: 2048,
			Output: caption},
	}

	rows, bodies := attachChildPayloads(ops, payloads, true)

	if strings.Contains(rows[0].Detail, "character(s)") {
		t.Errorf("row detail still carries the character count: %q", rows[0].Detail)
	}
	if !strings.Contains(rows[0].Detail, "nil.go:42") {
		t.Errorf("row detail = %q, want an excerpt of what the describer wrote", rows[0].Detail)
	}
	if !strings.Contains(bodies[0].response, caption) {
		t.Errorf("caption body = %q, want the full caption", bodies[0].response)
	}

	// The image is recorded by type and size — never its bytes, the rule the
	// parent capture follows.
	var req struct {
		Instruction string `json:"instruction"`
		Image       struct {
			MediaType string `json:"media_type"`
			Bytes     int    `json:"bytes"`
		} `json:"image"`
	}
	if err := json.Unmarshal([]byte(bodies[0].request), &req); err != nil {
		t.Fatalf("caption request is not the documented shape: %v (%q)", err, bodies[0].request)
	}
	if req.Image.MediaType != "image/png" || req.Image.Bytes != 2048 {
		t.Errorf("caption request image = %+v, want the type and size of the described image", req.Image)
	}
	if req.Instruction == "" {
		t.Error("caption request has no instruction — half of what the step did is the prompt it sent")
	}
}

// The security shape of this path: a caption is model output derived from an
// image, and an image is very often a screenshot of a terminal — the one place
// a credential is most likely to be transcribed verbatim into a log.
func TestAttachChildPayloads_RedactsASecretTranscribedInACaption(t *testing.T) {
	const secret = "sk-abcdefgh123456789"
	ops := []storage.ChildLog{{Kind: string(routing.OpImageCaption)}}
	payloads := []routing.ChildPayload{{
		Kind:   routing.OpImageCaption,
		Output: "a terminal showing `export OPENAI_API_KEY=" + secret + "` and a passing test run",
	}}

	rows, bodies := attachChildPayloads(ops, payloads, true)

	if strings.Contains(bodies[0].response, secret) {
		t.Errorf("caption body leaks the transcribed credential: %q", bodies[0].response)
	}
	if strings.Contains(rows[0].Detail, secret) {
		t.Errorf("the row excerpt leaks the transcribed credential: %q", rows[0].Detail)
	}
	// The rest of the description survives — redaction masks the credential,
	// it does not throw the description away.
	if !strings.Contains(bodies[0].response, "passing test run") {
		t.Errorf("caption body = %q, want the description minus the credential", bodies[0].response)
	}
}

func TestAttachChildPayloads_OpWithoutAPayloadKeepsItsDetail(t *testing.T) {
	ops := []storage.ChildLog{
		{Kind: string(routing.OpFailover), FromRef: "acc-a", ToRef: "acc-b", Detail: "chain failover after rate_limit"},
		{Kind: string(routing.OpCapabilityFallback), Detail: "vision"},
	}

	rows, bodies := attachChildPayloads(ops, nil, true)

	if rows[0].Detail != "chain failover after rate_limit" || rows[1].Detail != "vision" {
		t.Errorf("details were rewritten for ops with no payload: %+v", rows)
	}
	for i, b := range bodies {
		if b.request != "" || b.response != "" {
			t.Errorf("bodies[%d] = %+v, want empty (nothing was sent)", i, b)
		}
	}
}

// The capture switch covers the caption excerpt too. The excerpt IS caption
// text — model output derived from an image — and the row's detail is written by
// InsertChildLogs before the payload write's own gate runs, so an operator who
// turned body capture off must not still get image content in request_child_logs.
func TestAttachChildPayloads_CaptureOffStoresNoCaptionText(t *testing.T) {
	const caption = "a terminal window showing an internal hostname and a session cookie"
	ops := []storage.ChildLog{
		{Kind: string(routing.OpImageCaption), Detail: "71 character(s) described"},
		{Kind: string(routing.OpEmulatedTool), ToRef: "web_search", Detail: `query "x" → 1 result(s)`},
	}
	payloads := []routing.ChildPayload{
		{Kind: routing.OpImageCaption, Output: caption},
		{Kind: routing.OpEmulatedTool, Tool: "web_search", Input: `{"query":"x"}`, Output: `{"results":[]}`},
	}

	rows, bodies := attachChildPayloads(ops, payloads, false)

	if rows[0].Detail != "71 character(s) described" {
		t.Errorf("caption detail = %q, want the trace's character count when capture is off", rows[0].Detail)
	}
	if strings.Contains(rows[0].Detail, "session cookie") {
		t.Error("caption text reached the row detail with body capture disabled")
	}
	if rows[1].Detail != `query "x" → 1 result(s)` {
		t.Errorf("emulated-tool detail = %q, want the trace's summary untouched", rows[1].Detail)
	}
	for i, b := range bodies {
		if b.request != "" || b.response != "" {
			t.Errorf("bodies[%d] = %+v, want nothing rendered when capture is off", i, b)
		}
	}
}

// A caption longer than the row's allowance is cut for the ROW only; the payload
// keeps the whole thing, which is the point of opening the detail view.
func TestAttachChildPayloads_LongCaptionIsExcerptedOnTheRowOnly(t *testing.T) {
	long := strings.Repeat("very detailed observation. ", 40)
	ops := []storage.ChildLog{{Kind: string(routing.OpImageCaption)}}
	payloads := []routing.ChildPayload{{Kind: routing.OpImageCaption, Output: long}}

	rows, bodies := attachChildPayloads(ops, payloads, true)

	if len([]rune(rows[0].Detail)) > captionExcerptRunes+1 {
		t.Errorf("row detail = %d runes, want the excerpt cap (%d)", len([]rune(rows[0].Detail)), captionExcerptRunes)
	}
	if !strings.HasSuffix(rows[0].Detail, "…") {
		t.Errorf("a cut excerpt must show that it was cut: %q", rows[0].Detail)
	}
	if !strings.Contains(bodies[0].response, long) {
		t.Error("the body kept only the excerpt — the detail view must carry the whole caption")
	}
}
