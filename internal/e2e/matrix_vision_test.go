package e2e

// Vision matrix cell (vision__blind-chain-caption): a vision request rides a
// blind chain — a model whose capabilities claim vision but whose upstream
// rejects the image with a modality 400. The ADR-014/018 reactive rescue runs
// through the production wiring: the chain exhausts on capability_gap
// failures, the configured caption bootstrap (vision_fallback) describes the
// image, the description replaces it, and the SAME chain re-dispatches to a
// final answer. The learning write lands vision:false in the blind model row.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	openaiclient "github.com/openai/openai-go/v3"

	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/storage"
)

// testImageB64 is a complete 1x1 PNG, base64 — inline bytes a data: URI can
// carry, small enough to appear verbatim in every request it rides.
const testImageB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII="

// TestMatrix_Vision_BlindChainCaption drives the blind-chain rescue end to
// end: the OpenAI chat client attaches a data-URI image to an ordinary text
// question; the blind upstream answers the image-bearing dispatch with a
// 400 modality rejection (capability_gap), the caption model describes the
// image, the description takes the image's place and the blind chain answers.
func TestMatrix_Vision_BlindChainCaption(t *testing.T) {
	ctx := context.Background()
	// The blind account advertises allCaps — the operator-declared vision a
	// model can turn out to lack; its only channel back is the dispatch's
	// 400 (the ADR-014 premise, not an operator misconfiguration).
	blind := upstream{ID: "blind-up", Type: "custom-openai"}
	vision := upstream{ID: "vision-cap", Type: "custom-openai"}
	e := newMatrixEnv(t, blind, vision)
	// The blind model row exists to learn into: the gap resolves the chain
	// entry to the row's wire name, and the learning write lands there.
	if err := e.store.PutModel(ctx, storage.Model{ID: blind.ID, ProviderID: blind.ID, Name: "blind:v1"}); err != nil {
		t.Fatalf("seed model row: %v", err)
	}

	const (
		caption = "Caption: a browser page with an HTTP 500 banner and the text 12C sunny."
		final   = "Answer from the image: the page shows an HTTP 500 banner."
	)
	// Round 1: the image-bearing dispatch the blind model must reject.
	// Exactly once, so the caption call matches the next expectation.
	e.ms.Upsert(t, map[string]any{
		"httpRequest":  map[string]any{"method": "POST", "path": "/v1/chat/completions"},
		"httpResponse": map[string]any{"statusCode": 400, "body": `{"error":{"message":"model does not support image input","type":"invalid_request_error","code":"unsupported_content"}}`},
		"times":        map[string]any{"remainingTimes": 1, "unlimited": false},
	})
	// The caption call: the vision model sees only the image and the
	// instruction — never the conversation.
	e.ms.Upsert(t, map[string]any{
		"httpRequest": map[string]any{"method": "POST", "path": "/v1/chat/completions"},
		"httpLlmResponse": map[string]any{
			"provider":   "OPENAI",
			"model":      "vision:wire",
			"completion": map[string]any{"text": caption},
		},
		"times": map[string]any{"remainingTimes": 1, "unlimited": false},
	})
	// Round 2: the re-dispatch with the description in the image's place.
	e.ms.Upsert(t, map[string]any{
		"httpRequest": map[string]any{"method": "POST", "path": "/v1/chat/completions"},
		"httpLlmResponse": map[string]any{
			"provider":   "OPENAI",
			"model":      "blind:v1",
			"completion": map[string]any{"text": final},
		},
	})

	// The vision_fallback wiring: the caption model is the stored fallback
	// chain entry, fully resolved to (provider, wire model).
	e.exec.SetCapabilityFallbacks([]routing.ChainEntry{{ProviderID: vision.ID, Model: "vision:wire"}})

	resp, err := e.openai.Chat.Completions.New(ctx, openaiclient.ChatCompletionNewParams{
		Model: blind.ID,
		Messages: []openaiclient.ChatCompletionMessageParamUnion{
			openaiclient.UserMessage([]openaiclient.ChatCompletionContentPartUnionParam{
				openaiclient.TextContentPart("What does this screenshot show?"),
				openaiclient.ImageContentPart(openaiclient.ChatCompletionContentPartImageImageURLParam{
					URL: "data:image/png;base64," + testImageB64,
				}),
			}),
		},
	})
	if err != nil {
		t.Fatalf("chat completion: %v", err)
	}
	if len(resp.Choices) == 0 || !strings.Contains(resp.Choices[0].Message.Content, "HTTP 500 banner") {
		t.Fatalf("client answer carries no final text: %+v", resp.Choices)
	}

	// Three upstream dispatches: the rejected image round, the caption call,
	// the re-dispatch. The wire must show the image arriving only to the
	// caption model and never to the blind model's successful round.
	var chatRequests []map[string]any
	for _, r := range e.ms.Requests(t) {
		if r.Path == "/v1/chat/completions" {
			chatRequests = append(chatRequests, upstreamJSON(t, r))
		}
	}
	if len(chatRequests) != 3 {
		t.Fatalf("upstream /v1/chat/completions requests = %d, want 3 (blind dispatch, caption, re-dispatch)", len(chatRequests))
	}
	if got, _ := chatRequests[0]["model"].(string); got != "blind:v1" {
		t.Errorf("round-1 model = %q, want the row's wire name blind:v1", got)
	}
	if !strings.Contains(jsonMarshal(chatRequests[0]), testImageB64) {
		t.Errorf("round-1 request carries no image: %s", jsonMarshal(chatRequests[0]))
	}
	round2 := jsonMarshal(chatRequests[1])
	if got, _ := chatRequests[1]["model"].(string); got != "vision:wire" {
		t.Errorf("caption model = %q, want the fallback entry's wire name vision:wire", got)
	}
	if !strings.Contains(round2, testImageB64) {
		t.Errorf("caption call carries no image: %s", round2)
	}
	if !strings.Contains(round2, "Describe this image") {
		t.Errorf("caption call carries the instruction prompt: %s", round2)
	}
	round3 := jsonMarshal(chatRequests[2])
	if strings.Contains(round3, testImageB64) {
		t.Errorf("re-dispatch still carries the image: %s", round3)
	}
	if !strings.Contains(round3, caption) {
		t.Errorf("re-dispatch carries no description of the image: %s", round3)
	}

	// The learning write landed vision:false on the blind model row.
	row, err := e.store.GetModel(ctx, blind.ID)
	if err != nil {
		t.Fatalf("get model row: %v", err)
	}
	if !strings.Contains(string(row.Capabilities), `"vision":false`) {
		t.Fatalf("learned capability = %s, want vision:false on the blind row", row.Capabilities)
	}
}

// jsonMarshal renders one upstream request for content assertions.
func jsonMarshal(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}
