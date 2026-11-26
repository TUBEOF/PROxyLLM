package openai

// The chat wire's tool-result contract, derived from the OpenAI chat
// completions spec: an assistant message with tool_calls must be followed by
// one role:"tool" message per call, carrying its tool_call_id. Anthropic
// carries the same result inside a user message (tool_result block), often
// beside reminder text, so the encoder must split it out.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/translate/ir"
)

func TestChatRequest_AnthropicToolResultBecomesToolMessage(t *testing.T) {
	req := ir.Request{
		Model: "m",
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, Parts: []ir.Part{
				ir.ToolCallPart{ID: "call_tool_1", Name: "Bash", Input: json.RawMessage(`{"command":"cat notes.txt"}`)},
			}},
			// The Anthropic user-message shape: a tool_result block and a
			// trailing text block in the same message.
			{Role: ir.RoleUser, Parts: []ir.Part{
				ir.ToolResultPart{ToolUseID: "call_tool_1", Content: []ir.Part{
					ir.TextPart{Text: "PROXYLLM-LIVE-MARKER-7Q"},
				}},
				ir.TextPart{Text: "reminder text"},
			}},
		},
	}
	out, err := Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var wire struct {
		Messages []struct {
			Role       string          `json:"role"`
			ToolCallID string          `json:"tool_call_id"`
			Content    json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(wire.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (assistant, tool, user): %s", len(wire.Messages), out)
	}
	tool := wire.Messages[1]
	if tool.Role != "tool" {
		t.Errorf("tool result role = %q, want tool", tool.Role)
	}
	if tool.ToolCallID != "call_tool_1" {
		t.Errorf("tool_call_id = %q, want call_tool_1", tool.ToolCallID)
	}
	if !strings.Contains(string(tool.Content), "PROXYLLM-LIVE-MARKER-7Q") {
		t.Errorf("tool result content = %s, want the tool output", tool.Content)
	}
	rest := wire.Messages[2]
	if rest.Role != "user" || !strings.Contains(string(rest.Content), "reminder text") {
		t.Errorf("remaining user content = %s %s, want the reminder text", rest.Role, rest.Content)
	}
}

func TestChatRequest_MultipleToolResultsOnly(t *testing.T) {
	req := ir.Request{
		Model: "m",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{
				ir.ToolResultPart{ToolUseID: "call_a", Content: []ir.Part{ir.TextPart{Text: "A"}}},
				ir.ToolResultPart{ToolUseID: "call_b", Content: []ir.Part{ir.TextPart{Text: "B"}}},
			}},
		},
	}
	out, err := Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var wire struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(wire.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 tool messages only: %s", len(wire.Messages), out)
	}
	for i, want := range []string{"call_a", "call_b"} {
		if wire.Messages[i].Role != "tool" || wire.Messages[i].ToolCallID != want {
			t.Errorf("message %d = %+v, want role tool with id %s", i, wire.Messages[i], want)
		}
	}
}
