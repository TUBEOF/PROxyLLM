package httpapi

// Spec: Logs-page redesign, conversation derivation
// priority order — Anthropic metadata.user_id session segment > whole
// user_id (probed from the RAW wire body: the codecs have no IR field for
// it), explicit headers (sanitized), previous_response_id, fallback hash
// (client key ‖ normalized system/first-user text). Stored id: conv_ +
// 8 hex of sha256(convKey). Only rows post-patch carry ids.

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// TestConversationKey_SessionSegment pins the Claude Code user_id shape:
// user_<hash>_account_<uuid>_session_<uuid> → the session segment wins.
func TestConversationKey_SessionSegment(t *testing.T) {
	a := &api{}
	raw := []byte(`{"metadata":{"user_id":"user_abc123def_account_9f8e7d6c-58a1-4b2c-9d0e-1a2b3c4d5e6f_session_11223344-5566-7788-99aa-bbccddeeff00"}}`)
	req := ir.Request{Model: "claude-sonnet-4-5"}
	r := httptest.NewRequest("POST", "/v1/messages", nil)

	key := a.conversationKey(r, protocolAnthropic, raw, req)
	if !strings.HasPrefix(key, "sess:11223344-5566-7788-99aa-bbccddeeff00") {
		t.Errorf("session segment hint = %q", key)
	}
	id := convIDFromKey(key)
	if !strings.HasPrefix(id, "conv_") || len(id) != 5+8 {
		t.Errorf("conv id %q malformed", id)
	}
	// Stability: the same hint hashes identically across turns.
	if again := convIDFromKey(a.conversationKey(r, protocolAnthropic, raw, req)); again != id {
		t.Errorf("unstable derivation: %q vs %q", again, id)
	}
}

// A metadata.user_id without a session segment hashes as uid: with the
// hint sanitized (arbitrary client text must not leak weird characters
// into the stored key).
func TestConversationKey_UserIDWithoutSession(t *testing.T) {
	a := &api{}
	raw := []byte(`{"metadata":{"user_id":"u 777/!"}}`)
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	key := a.conversationKey(r, protocolAnthropic, raw, ir.Request{})
	if !strings.HasPrefix(key, "uid:u777") {
		t.Errorf("user_id hint = %q, want sanitized uid: prefix", key)
	}
}

// An explicit header hint wins when no metadata rides the body.
func TestConversationKey_HeaderHint(t *testing.T) {
	a := &api{}
	req := ir.Request{Model: "m", Messages: []ir.Message{{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "hi"}}}}}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("X-Conversation-Id", "my-chain-42")

	key := a.conversationKey(r, protocolOpenAI, nil, req)
	if key != "hdr:my-chain-42" {
		t.Errorf("header hint = %q, want hdr:my-chain-42", key)
	}
	if id := convIDFromKey(key); !strings.HasPrefix(id, "conv_") || len(id) != 13 {
		t.Errorf("conv id = %q", id)
	}
}

// Header hints sanitize: newlines/spaces/NULs/slashes never enter the key.
func TestConversationKey_HeaderSanitized(t *testing.T) {
	a := &api{}
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set("X-Conversation-Id", "bad\nid with spaces/and\x00more!")

	key := a.conversationKey(r, protocolAnthropic, nil, ir.Request{})
	// Sanitized: only [A-Za-z0-9_-] survive (charset filter caps at 64).
	if key != "hdr:badidwithspacesandmore" {
		t.Errorf("unsanitized header hint: %q", key)
	}
}

// previous_response_id on the Responses surface links the turn.
func TestConversationKey_PreviousResponseID(t *testing.T) {
	a := &api{}
	raw := []byte(`{"model":"m","previous_response_id":"resp_abc123"}`)
	r := httptest.NewRequest("POST", "/v1/responses", nil)
	key := a.conversationKey(r, protocolOpenAIResponses, raw, ir.Request{})
	if key != "prv:resp_abc123" {
		t.Errorf("previous_response_id hint = %q, want prv:resp_abc123", key)
	}
}

// No hints → the fallback: normalized system text (whitespace collapsed),
// client-key scoped. Empty request → no key at all.
func TestConversationKey_SystemFallback(t *testing.T) {
	a := &api{}
	req := ir.Request{
		System:   []ir.TextPart{{Text: "You are a helpful  assistant"}},
		Messages: []ir.Message{{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "hi"}}}},
	}
	r := httptest.NewRequest("POST", "/v1/messages", nil)

	key1 := a.conversationKey(r, protocolAnthropic, nil, req)
	req2 := ir.Request{
		System:   []ir.TextPart{{Text: "You are a helpful assistant"}},
		Messages: req.Messages,
	}
	key2 := a.conversationKey(r, protocolAnthropic, nil, req2)
	if key1 != key2 {
		t.Errorf("normalization missed: %q vs %q", key1, key2)
	}
	if id := convIDFromKey(key1); !strings.HasPrefix(id, "conv_") || len(id) != 13 {
		t.Errorf("fallback id = %q", id)
	}
	if empty := a.conversationKey(r, protocolAnthropic, nil, ir.Request{}); empty != "" {
		t.Errorf("empty request key = %q, want \"\"", empty)
	}
}

// Different system prompts never share a conv key.
func TestConversationKey_DifferentSystemsDiffer(t *testing.T) {
	a := &api{}
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	k1 := a.conversationKey(r, protocolAnthropic, nil, ir.Request{System: []ir.TextPart{{Text: "alpha"}}})
	k2 := a.conversationKey(r, protocolAnthropic, nil, ir.Request{System: []ir.TextPart{{Text: "beta"}}})
	if k1 == k2 {
		t.Error("different system prompts must not share a conv key")
	}
}
