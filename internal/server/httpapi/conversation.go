package httpapi

// Conversation identity for the request log (Logs-page redesign):
// rows of one logical conversation chain in the WebUI gutter
// and earn the healed marker on retries. Derivation ORDER (first hit
// wins), unit-tested in conversation_test.go:
//
//  1. Anthropic surface: metadata.user_id from the RAW wire body (Claude
//     Code sends it; the wire struct deliberately has no metadata field —
//     the codecs would drop it, so the logmeta layer probes the raw body
//     itself). The `session_<…>` segment of
//     `user_<hash>_account_<uuid>_session_<uuid>` is the strongest hint,
//     else the whole user_id hashes.
//  2. Any surface: X-Conversation-Id / X-Session-Id headers (sanitized,
//     capped) — the scripted-client escape hatch.
//  3. OpenAI Responses surface: previous_response_id from the raw body
//     (turns of one chain; the rolling window makes the key stable).
//  4. Fallback: hash over client key ‖ normalized system text (or the
//     first user text) — every turn of a CLI conversation re-sends the
//     same system prompt, so turns of one conversation group per key while
//     different keys never collide.
//
// The STORED id is conv_ + first 8 hex of sha256(convKey); convKey itself
// persists (column conv_key) so the rolling-window lookup
// (storage.RecentConversation, 30 min) reuses the prior conversation_id
// instead of re-hashing. Rows written before the change carry no id —
// there is no backfill.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"github.com/tubeof/proxyllm/internal/auth"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// sessionSegment matches the session part of Claude Code's
// metadata.user_id (user_<hash>_account_<uuid>_session_<uuid>).
var sessionSegment = regexp.MustCompile(`session_([0-9a-fA-F-]{8,64})`)

// conversationKey produces the deterministic derivation input for a
// request. "" is valid (no usable hint — the row simply gets no id).
func (a *api) conversationKey(r *http.Request, protocol string, raw []byte, req ir.Request) string {
	if hint := conversationHint(r, protocol, raw); hint != "" {
		return hint
	}
	// 4: the fallback — hash over client key ‖ normalized system text (or
	// first user text). The "sys:" namespace keeps it disjoint from the
	// hint namespaces.
	if text := systemFallbackText(req); text != "" {
		return "sys:" + hashInput(clientKeyIDOf(r), text)
	}
	return ""
}

// conversationHint returns the raw priority hint (steps 1-3, first hit
// wins; "" when none). Steps 1/3 probe the raw wire body: the surface
// codecs have no IR field for metadata/previous_response_id, so the values
// would otherwise drop with the documented degradation.
func conversationHint(r *http.Request, protocol string, raw []byte) string {
	// 1: Anthropic metadata.user_id.
	if protocol == protocolAnthropic && len(raw) > 0 {
		var probe struct {
			Metadata struct {
				UserID string `json:"user_id"`
			} `json:"metadata"`
		}
		if json.Unmarshal(raw, &probe) == nil && probe.Metadata.UserID != "" {
			if m := sessionSegment.FindStringSubmatch(probe.Metadata.UserID); m != nil {
				return "sess:" + m[1]
			}
			return "uid:" + sanitizeHint(probe.Metadata.UserID)
		}
	}

	// 2: explicit headers, sanitized (length cap + charset filter).
	for _, h := range []string{"X-Conversation-Id", "X-Session-Id"} {
		if v := r.Header.Get(h); v != "" {
			if s := sanitizeHint(v); s != "" {
				return "hdr:" + s
			}
		}
	}

	// 3: OpenAI Responses previous_response_id.
	if protocol == protocolOpenAIResponses && len(raw) > 0 {
		var probe struct {
			PreviousResponseID string `json:"previous_response_id"`
		}
		if json.Unmarshal(raw, &probe) == nil && probe.PreviousResponseID != "" {
			if s := sanitizeHint(probe.PreviousResponseID); s != "" {
				return "prv:" + s
			}
		}
	}
	return ""
}

// hashInput is the shared sha256 hex digest (16 hex chars — enough entropy
// for a per-tenant grouping key, short enough to index).
func hashInput(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}

// clientKeyIDOf resolves the authed client key id for an incoming request
// ("" without the auth middleware — pre-auth unit tests).
func clientKeyIDOf(r *http.Request) string {
	return auth.ClientKeyIDFromContext(r.Context())
}

// sanitizeHint keeps [A-Za-z0-9_-], caps at 64 chars.
func sanitizeHint(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 64 {
		v = v[:64]
	}
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// convIDFromKey derives the stored/displayed id: conv_ + first 8 hex of
// sha256(convKey) — the wire shape the WebUI conversation column renders.
func convIDFromKey(convKey string) string {
	sum := sha256.Sum256([]byte(convKey))
	return "conv_" + hex.EncodeToString(sum[:4])
}

// systemFallbackText renders the step-4 fallback input: the normalized
// system text, or the first user text when no system block exists (some
// CLI clients put identity there). Normalization = trim + collapse inner
// whitespace (unicode.IsSpace) — font-identical prompts with differing
// spacing hash the same.
func systemFallbackText(req ir.Request) string {
	var parts []string
	for _, sp := range req.System {
		if sp.Text != "" {
			parts = append(parts, sp.Text)
		}
	}
	if len(parts) == 0 {
		for _, msg := range req.Messages {
			if msg.Role != ir.RoleUser {
				continue
			}
			for _, part := range msg.Parts {
				if tp, ok := part.(ir.TextPart); ok && tp.Text != "" {
					parts = append(parts, tp.Text)
					break
				}
			}
			if len(parts) > 0 {
				break
			}
		}
	}
	return normalizeSpace(strings.Join(parts, "\x00"))
}

// normalizeSpace trims and collapses inner whitespace runs to one space.
func normalizeSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !lastSpace && b.Len() > 0 {
				b.WriteByte(' ')
			}
			lastSpace = true
			continue
		}
		lastSpace = false
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}
