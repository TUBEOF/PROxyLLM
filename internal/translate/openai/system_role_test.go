package openai_test

import (
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/translate/ir"
	oa "github.com/tubeof/proxyllm/internal/translate/openai"
)

// TestEncodeRequest_SystemRoleMessageEncodesEmptyStringContent: a
// role:"system" message inside messages — Claude Code appends one for
// SessionStart-hook output and injected reminders — decoded by the
// Anthropic codec into ir.RoleSystem. The OpenAI chat encoder must
// encode it as a real system message, never fall through every role
// case leaving the zero-value wireContent that marshals as
// "content":null — which Ollama's OpenAI-compatibility endpoint rejects
// with HTTP 400 "invalid message content type: <nil>" (same class as
// TestEncodeRequest_ToolCallOnlyAssistantEncodesEmptyStringContent).
func TestEncodeRequest_SystemRoleMessageEncodesEmptyStringContent(t *testing.T) {
	req := ir.Request{
		Model: "glm-5.3",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "hello"}}},
			{Role: ir.RoleSystem, Parts: []ir.Part{ir.TextPart{Text: "SessionStart hook output"}}},
		},
	}
	raw, err := oa.Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	body := string(raw)
	if strings.Contains(body, `"content":null`) {
		t.Errorf("encoded body contains \"content\":null for the role=system message — Ollama's OpenAI-compat endpoint answers 400 invalid message content type: <nil>: %s", body)
	}
	// The system message must survive as a system-role message carrying
	// its text — not be dropped or turned into something else.
	if !strings.Contains(body, `"role":"system"`) || !strings.Contains(body, "SessionStart hook output") {
		t.Errorf("role=system message lost or unmapped in encoded body: %s", body)
	}

}
