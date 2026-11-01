package e2e

// Conformance matrix, first slice: every existing client surface against the
// provider wires it can reach, with MockServer producing the provider's real
// response shape. Each test asserts the client-visible answer AND the exact
// request the upstream received, so a passing cell proves both directions of
// the translation.

import (
	"context"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	openaiclient "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

// upstreamBody returns the JSON body of the single upstream request for path.
func (e *matrixEnv) upstreamBody(t *testing.T, path string) map[string]any {
	t.Helper()
	var hits []int
	reqs := e.ms.Requests(t)
	for i, r := range reqs {
		if r.Path == path {
			hits = append(hits, i)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("upstream requests for %s = %d, want exactly 1 (all: %+v)", path, len(hits), reqs)
	}
	return upstreamJSON(t, reqs[hits[0]])
}

// 1. OpenAI Chat client → Ollama Cloud (OpenAI-compatible upstream), streaming.
func TestMatrix_ChatClient_OllamaCloudUpstream_Streaming(t *testing.T) {
	ctx := context.Background()
	up := upstream{ID: "ollama-cloud-up", Type: "ollama-cloud"}
	e := newMatrixEnv(t, up)
	e.ms.Upsert(t, expectation("OPENAI", up.ID, "/v1/chat/completions",
		chatCompletion("Hello from the matrix", true)))

	stream := e.openai.Chat.Completions.NewStreaming(ctx, openaiclient.ChatCompletionNewParams{
		Model:    up.ID,
		Messages: []openaiclient.ChatCompletionMessageParamUnion{openaiclient.UserMessage("hi")},
	})
	defer func() { _ = stream.Close() }()
	var text strings.Builder
	for stream.Next() {
		if choices := stream.Current().Choices; len(choices) > 0 {
			text.WriteString(choices[0].Delta.Content)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("chat stream: %v", err)
	}
	if !strings.Contains(text.String(), "Hello from the matrix") {
		t.Fatalf("client text = %q, want the simulated upstream answer", text.String())
	}

	body := e.upstreamBody(t, "/v1/chat/completions")
	if body["model"] != up.ID {
		t.Errorf("upstream model = %v, want %s", body["model"], up.ID)
	}
	if body["stream"] != true {
		t.Errorf("upstream stream = %v, want true (the client asked to stream)", body["stream"])
	}
	if msgs, _ := body["messages"].([]any); len(msgs) != 1 {
		t.Errorf("upstream messages = %v, want the client's single user turn", body["messages"])
	}
}

// 2. OpenAI Responses client → OpenAI Responses upstream, non-streaming.
func TestMatrix_ResponsesClient_ResponsesUpstream(t *testing.T) {
	ctx := context.Background()
	up := upstream{ID: "oa-responses-up", Type: "openai-responses"}
	e := newMatrixEnv(t, up)
	e.ms.Upsert(t, expectation("OPENAI_RESPONSES", up.ID, "/v1/responses",
		chatCompletion("Responses upstream ok", false)))

	resp, err := e.openai.Responses.New(ctx, responses.ResponseNewParams{
		Model: up.ID,
		Input: responses.ResponseNewParamsInputUnion{OfString: openaiclient.String("hi")},
	})
	if err != nil {
		t.Fatalf("responses create: %v", err)
	}
	if !strings.Contains(resp.OutputText(), "Responses upstream ok") {
		t.Fatalf("output_text = %q, want the simulated upstream answer", resp.OutputText())
	}

	body := e.upstreamBody(t, "/v1/responses")
	if body["model"] != up.ID {
		t.Errorf("upstream model = %v, want %s", body["model"], up.ID)
	}
}

// 3. Anthropic Messages client → Anthropic upstream, streaming.
func TestMatrix_AnthropicClient_AnthropicUpstream_Streaming(t *testing.T) {
	ctx := context.Background()
	up := upstream{ID: "anthropic-up", Type: "anthropic"}
	e := newMatrixEnv(t, up)
	e.ms.Upsert(t, expectation("ANTHROPIC", up.ID, "/v1/messages",
		chatCompletion("Anthropic upstream ok", true)))

	stream := e.anthrop.Messages.NewStreaming(ctx, anthropic.MessageNewParams{
		MaxTokens: 256,
		Model:     up.ID,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	})
	defer func() { _ = stream.Close() }()
	var text strings.Builder
	for stream.Next() {
		ev := stream.Current()
		if ev.Type == "content_block_delta" && ev.Delta.Type == "text_delta" {
			text.WriteString(ev.Delta.Text)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("messages stream: %v", err)
	}
	if !strings.Contains(text.String(), "Anthropic upstream ok") {
		t.Fatalf("client text = %q, want the simulated upstream answer", text.String())
	}

	body := e.upstreamBody(t, "/v1/messages")
	if body["model"] != up.ID {
		t.Errorf("upstream model = %v, want %s", body["model"], up.ID)
	}
	if body["stream"] != true {
		t.Errorf("upstream stream = %v, want true", body["stream"])
	}
}

// 4. Cross-protocol: OpenAI Chat client → Anthropic upstream, non-streaming.
func TestMatrix_ChatClient_AnthropicUpstream(t *testing.T) {
	ctx := context.Background()
	up := upstream{ID: "anthropic-cross", Type: "anthropic"}
	e := newMatrixEnv(t, up)
	e.ms.Upsert(t, expectation("ANTHROPIC", up.ID, "/v1/messages",
		chatCompletion("Cross-protocol answer", false)))

	cc, err := e.openai.Chat.Completions.New(ctx, openaiclient.ChatCompletionNewParams{
		Model:    up.ID,
		Messages: []openaiclient.ChatCompletionMessageParamUnion{openaiclient.UserMessage("hi")},
	})
	if err != nil {
		t.Fatalf("chat completion: %v", err)
	}
	if len(cc.Choices) == 0 || !strings.Contains(cc.Choices[0].Message.Content, "Cross-protocol answer") {
		t.Fatalf("client answer = %+v, want the Anthropic upstream's text", cc.Choices)
	}

	body := e.upstreamBody(t, "/v1/messages")
	if body["model"] != up.ID {
		t.Errorf("upstream model = %v, want %s", body["model"], up.ID)
	}
	if body["max_tokens"] == nil {
		t.Errorf("upstream body carries no max_tokens: %v", body)
	}
}

// 5. Cross-protocol: OpenAI Responses client → Anthropic upstream.
func TestMatrix_ResponsesClient_AnthropicUpstream(t *testing.T) {
	ctx := context.Background()
	up := upstream{ID: "anthropic-resp-cross", Type: "anthropic"}
	e := newMatrixEnv(t, up)
	e.ms.Upsert(t, expectation("ANTHROPIC", up.ID, "/v1/messages",
		chatCompletion("Responses to Messages answer", false)))

	resp, err := e.openai.Responses.New(ctx, responses.ResponseNewParams{
		Model: up.ID,
		Input: responses.ResponseNewParamsInputUnion{OfString: openaiclient.String("hi")},
	})
	if err != nil {
		t.Fatalf("responses create: %v", err)
	}
	if !strings.Contains(resp.OutputText(), "Responses to Messages answer") {
		t.Fatalf("output_text = %q, want the Anthropic upstream's text", resp.OutputText())
	}
}
