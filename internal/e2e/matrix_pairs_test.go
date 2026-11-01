package e2e

// The pair matrix: every green cell from the manifest runs as its own
// subtest — client surface × upstream wire × feature × mode. The manifest is
// the single source of truth (testdata/matrix.json); this runner only
// executes it. Adding a combination means editing the manifest, never
// writing another bespoke test.

import (
	"bufio"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	openaiclient "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

// pairUpstream maps a manifest upstream id onto the provider spec.
func pairUpstream(cell matrixCell) upstream {
	types := map[string]string{
		"openai-chat":        "custom-openai",
		"openai-responses":   "openai-responses",
		"anthropic-messages": "anthropic",
		"ollama-native":      "ollama-native",
		"ollama-cloud":       "ollama-cloud",
	}
	typ, ok := types[cell.Upstream]
	if !ok {
		return upstream{ID: cell.Upstream, Type: "unknown-" + cell.Upstream}
	}
	return upstream{ID: cell.Upstream, Type: typ}
}

// pairWire names the MockServer provider and path for one upstream.
func pairWire(upstreamID string) (provider, path string) {
	switch upstreamID {
	case "openai-chat", "ollama-cloud":
		return "OPENAI", "/v1/chat/completions"
	case "openai-responses":
		return "OPENAI_RESPONSES", "/v1/responses"
	case "anthropic-messages":
		return "ANTHROPIC", "/v1/messages"
	case "ollama-native":
		return "OLLAMA", "/api/chat"
	}
	return "UNKNOWN", "/unknown"
}

// toolCallArgs is the argument payload every tools cell must see.
const toolName = "get_weather"
const toolArgs = `{"city":"Paris"}`

// TestMatrix_TextPairs runs every green text cell: the client's answer must
// carry the cell's unique marker and the upstream must have received the
// client's turn addressed to the right model.
func TestMatrix_TextPairs(t *testing.T) {
	for _, cell := range greenPairCells(t, "text") {
		t.Run(cell.ID, func(t *testing.T) {
			runPairCell(t, cell, map[string]any{
				"text":      "answer:" + cell.ID,
				"streaming": cell.Mode == "stream",
			})
		})
	}
}

// TestMatrix_ToolPairs runs every green tools cell: the client must receive
// the function call with its arguments, and the upstream must have received
// the declared tool.
func TestMatrix_ToolPairs(t *testing.T) {
	for _, cell := range greenPairCells(t, "tools") {
		t.Run(cell.ID, func(t *testing.T) {
			provider, _ := pairWire(cell.Upstream)
			stopReason := map[string]string{
				"OPENAI":           "tool_calls",
				"OPENAI_RESPONSES": "tool_calls",
				"ANTHROPIC":        "tool_use",
				"OLLAMA":           "",
			}[provider]
			completion := map[string]any{
				"streaming": cell.Mode == "stream",
				"toolCalls": []map[string]any{{
					"id":        "call_pair_1",
					"name":      toolName,
					"arguments": toolArgs,
				}},
			}
			if stopReason != "" {
				completion["stopReason"] = stopReason
			}
			runPairCell(t, cell, completion)
		})
	}
}

// runPairCell assembles the cell's environment, installs the completion and
// asserts both directions.
func runPairCell(t *testing.T, cell matrixCell, completion map[string]any) {
	t.Helper()
	wantText := "answer:" + cell.ID
	if cell.Feature == "text" {
		completion["text"] = wantText
	}

	e := newMatrixEnv(t, pairUpstream(cell))
	provider, path := pairWire(cell.Upstream)
	e.ms.Upsert(t, expectation(provider, cell.Upstream, path, completion))

	res := drivePairClient(t, e, cell)

	if cell.Feature == "text" {
		if !strings.Contains(res.text, wantText) {
			t.Fatalf("%s: client text = %q, want %q", cell.ID, res.text, wantText)
		}
	} else {
		if res.toolName != toolName || !strings.Contains(res.toolArgs, "Paris") {
			t.Fatalf("%s: tool call = %s/%s, want %s with the Paris arguments",
				cell.ID, res.toolName, res.toolArgs, toolName)
		}
	}

	body := e.upstreamBody(t, path)
	if body["model"] != cell.Upstream {
		t.Errorf("%s: upstream model = %v, want %s", cell.ID, body["model"], cell.Upstream)
	}
	if cell.Feature == "tools" {
		if tools, _ := body["tools"].([]any); len(tools) == 0 {
			t.Errorf("%s: upstream request carries no tools: %v", cell.ID, body)
		}
	}
}

// pairResult is what a client driver reports back.
type pairResult struct {
	text     string
	toolName string
	toolArgs string
}

// drivePairClient performs the cell's client request and collects the answer
// (text or the function call).
func drivePairClient(t *testing.T, e *matrixEnv, cell matrixCell) pairResult {
	t.Helper()
	streaming := cell.Mode == "stream"
	withTools := cell.Feature == "tools"

	switch cell.Client {
	case "openai-chat":
		return driveChatClient(t, e, cell.Upstream, streaming, withTools)
	case "openai-responses":
		return driveResponsesClient(t, e, cell.Upstream, streaming, withTools)
	case "anthropic-messages":
		return driveAnthropicClient(t, e, cell.Upstream, streaming, withTools)
	case "ollama-native":
		return driveOllamaClient(t, e, cell.Upstream, streaming, withTools)
	default:
		t.Fatalf("no driver for client %q", cell.Client)
		return pairResult{}
	}
}

// --- openai-chat ---

func chatParams(model string, withTools bool) openaiclient.ChatCompletionNewParams {
	params := openaiclient.ChatCompletionNewParams{
		Model:    model,
		Messages: []openaiclient.ChatCompletionMessageParamUnion{openaiclient.UserMessage("hi")},
	}
	if withTools {
		params.Tools = []openaiclient.ChatCompletionToolUnionParam{{
			OfFunction: &openaiclient.ChatCompletionFunctionToolParam{
				Function: openaiclient.FunctionDefinitionParam{
					Name:       toolName,
					Parameters: map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
				},
			},
		}}
	}
	return params
}

func driveChatClient(t *testing.T, e *matrixEnv, model string, streaming, withTools bool) pairResult {
	t.Helper()
	ctx := context.Background()
	params := chatParams(model, withTools)

	if !streaming {
		cc, err := e.openai.Chat.Completions.New(ctx, params)
		if err != nil {
			t.Fatalf("chat completion: %v", err)
		}
		if len(cc.Choices) == 0 {
			t.Fatalf("chat completion has no choices: %+v", cc)
		}
		out := pairResult{text: cc.Choices[0].Message.Content}
		if len(cc.Choices[0].Message.ToolCalls) > 0 {
			call := cc.Choices[0].Message.ToolCalls[0]
			out.toolName, out.toolArgs = call.Function.Name, call.Function.Arguments
		}
		return out
	}

	stream := e.openai.Chat.Completions.NewStreaming(ctx, params)
	defer func() { _ = stream.Close() }()
	var text strings.Builder
	args := map[int]strings.Builder{}
	lastName := ""
	for stream.Next() {
		chunk := stream.Current()
		if len(chunk.Choices) == 0 {
			continue
		}
		text.WriteString(chunk.Choices[0].Delta.Content)
		for _, call := range chunk.Choices[0].Delta.ToolCalls {
			if call.Function.Name != "" {
				lastName = call.Function.Name
			}
			b := args[int(call.Index)]
			b.WriteString(call.Function.Arguments)
			args[int(call.Index)] = b
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("chat stream: %v", err)
	}
	var joinedArgs string
	for _, b := range args {
		joinedArgs += b.String()
	}
	return pairResult{text: text.String(), toolName: lastName, toolArgs: joinedArgs}
}

// --- openai-responses ---

func responsesParams(model string, withTools bool) responses.ResponseNewParams {
	params := responses.ResponseNewParams{
		Model: model,
		Input: responses.ResponseNewParamsInputUnion{OfString: openaiclient.String("hi")},
	}
	if withTools {
		params.Tools = []responses.ToolUnionParam{{
			OfFunction: &responses.FunctionToolParam{
				Name:       toolName,
				Parameters: map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
				Strict:     openaiclient.Bool(false),
			},
		}}
	}
	return params
}

func driveResponsesClient(t *testing.T, e *matrixEnv, model string, streaming, withTools bool) pairResult {
	t.Helper()
	ctx := context.Background()
	params := responsesParams(model, withTools)

	if !streaming {
		resp, err := e.openai.Responses.New(ctx, params)
		if err != nil {
			t.Fatalf("responses create: %v", err)
		}
		raw, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("marshal responses result: %v", err)
		}
		return parseResponsesJSON(t, raw)
	}

	stream := e.openai.Responses.NewStreaming(ctx, params)
	defer func() { _ = stream.Close() }()
	var text strings.Builder
	var doneItem string
	for stream.Next() {
		ev := stream.Current()
		switch ev.Type {
		case "response.output_text.delta":
			text.WriteString(ev.Delta)
		case "response.output_item.done":
			doneItem = ev.AsResponseOutputItemDone().RawJSON()
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("responses stream: %v", err)
	}
	out := pairResult{text: text.String()}
	if doneItem != "" {
		var item struct {
			Item struct {
				Type      string `json:"type"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"item"`
		}
		if json.Unmarshal([]byte(doneItem), &item) == nil && item.Item.Type == "function_call" {
			out.toolName, out.toolArgs = item.Item.Name, item.Item.Arguments
		}
	}
	return out
}

// parseResponsesJSON reads a Responses result (SDK-marshaled) for the text
// and the function_call item, decoupled from SDK union accessors.
func parseResponsesJSON(t *testing.T, raw []byte) pairResult {
	t.Helper()
	var doc struct {
		OutputText string `json:"output_text"`
		Output     []struct {
			Type      string          `json:"type"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse responses result: %v (%s)", err, raw)
	}
	out := pairResult{}
	for _, item := range doc.Output {
		if item.Type == "function_call" {
			out.toolName, out.toolArgs = item.Name, string(item.Arguments)
		}
		for _, c := range item.Content {
			if c.Type == "output_text" {
				out.text += c.Text
			}
		}
	}
	if out.text == "" && doc.OutputText != "" {
		out.text = doc.OutputText
	}
	return out
}

// --- anthropic-messages ---

func anthropicParams(model string, withTools bool) anthropic.MessageNewParams {
	params := anthropic.MessageNewParams{
		MaxTokens: 256,
		Model:     model,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	}
	if withTools {
		params.Tools = []anthropic.ToolUnionParam{{
			OfTool: &anthropic.ToolParam{
				Name: toolName,
				InputSchema: anthropic.ToolInputSchemaParam{
					Properties: map[string]any{"city": map[string]any{"type": "string"}},
				},
			},
		}}
	}
	return params
}

func driveAnthropicClient(t *testing.T, e *matrixEnv, model string, streaming, withTools bool) pairResult {
	t.Helper()
	ctx := context.Background()
	params := anthropicParams(model, withTools)

	if !streaming {
		msg, err := e.anthrop.Messages.New(ctx, params)
		if err != nil {
			t.Fatalf("messages create: %v", err)
		}
		raw, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal messages result: %v", err)
		}
		return parseAnthropicJSON(t, raw)
	}

	stream := e.anthrop.Messages.NewStreaming(ctx, params)
	defer func() { _ = stream.Close() }()
	var text, toolNameOut, toolArgs strings.Builder
	for stream.Next() {
		ev := stream.Current()
		switch ev.Type {
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" {
				text.WriteString(ev.Delta.Text)
			}
			if ev.Delta.Type == "input_json_delta" {
				var raw struct {
					Delta struct {
						PartialJSON string `json:"partial_json"`
					} `json:"delta"`
				}
				if json.Unmarshal([]byte(ev.RawJSON()), &raw) == nil {
					toolArgs.WriteString(raw.Delta.PartialJSON)
				}
			}
		case "content_block_start":
			var raw struct {
				ContentBlock struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			if json.Unmarshal([]byte(ev.RawJSON()), &raw) == nil && raw.ContentBlock.Type == "tool_use" {
				toolNameOut.WriteString(raw.ContentBlock.Name)
			}
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("messages stream: %v", err)
	}
	return pairResult{text: text.String(), toolName: toolNameOut.String(), toolArgs: toolArgs.String()}
}

// parseAnthropicJSON reads an Anthropic result for text and tool_use blocks.
func parseAnthropicJSON(t *testing.T, raw []byte) pairResult {
	t.Helper()
	var doc struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse messages result: %v (%s)", err, raw)
	}
	out := pairResult{}
	for _, block := range doc.Content {
		switch block.Type {
		case "text":
			out.text += block.Text
		case "tool_use":
			out.toolName = block.Name
			out.toolArgs = string(block.Input)
		}
	}
	return out
}

// --- ollama-native (raw HTTP) ---

func driveOllamaClient(t *testing.T, e *matrixEnv, model string, streaming, withTools bool) pairResult {
	t.Helper()
	body := map[string]any{
		"model":    model,
		"stream":   streaming,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}
	if withTools {
		body["tools"] = []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name":       toolName,
				"parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
			},
		}}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal ollama request: %v", err)
	}
	resp := e.ollamaPost(t, string(raw))
	if resp.StatusCode != 200 {
		t.Fatalf("ollama request status = %d, want 200", resp.StatusCode)
	}

	var text strings.Builder
	var name, args string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var line struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		text.WriteString(line.Message.Content)
		if len(line.Message.ToolCalls) > 0 {
			name = line.Message.ToolCalls[0].Function.Name
			args = string(line.Message.ToolCalls[0].Function.Arguments)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read ollama stream: %v", err)
	}
	return pairResult{text: text.String(), toolName: name, toolArgs: args}
}
