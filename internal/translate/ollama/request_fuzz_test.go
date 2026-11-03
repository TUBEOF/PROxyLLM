package ollama_test

// WP8: no panic encoding arbitrary IR; the codec must either produce wire
// bytes or return an error, never crash.

import (
	"encoding/json"
	"testing"

	"github.com/tubeof/proxyllm/internal/translate/ir"
	ollama "github.com/tubeof/proxyllm/internal/translate/ollama"
)

func FuzzOllamaEncodeRequest(f *testing.F) {
	f.Add(`{"query":"hi"}`)
	f.Add(`{"url":"https://example.test"}`)
	f.Add(`{"image_ref":"[Bild 1]"}`)
	f.Add(`not json at all`)
	for rank, seed := range []ir.Request{
		{Model: "m", Messages: []ir.Message{{Role: ir.RoleUser, Parts: []ir.Part{ir.TextPart{Text: "hi"}}}}},
		{Model: "m", Messages: []ir.Message{{Role: ir.RoleAssistant, Parts: []ir.Part{ir.ToolCallPart{ID: "c1", Name: "web_search", Input: json.RawMessage(`{"query":"wetter"}`)}}}}},
		{Model: "m", Tools: []ir.Tool{{Name: "web_search", Emulated: true}}},
	} {
		raw, err := json.Marshal(seed)
		if err == nil {
			f.Add(string(raw))
		}
		_ = rank
	}
	f.Fuzz(func(t *testing.T, reqJSON string) {
		var req ir.Request
		if json.Unmarshal([]byte(reqJSON), &req) != nil {
			return
		}
		_, _ = ollama.Codec{}.EncodeRequest(req)
	})
}
