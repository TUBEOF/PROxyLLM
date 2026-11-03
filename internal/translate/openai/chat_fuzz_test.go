package openai_test

// WP8 slice 4: the chat request decoder against arbitrary bytes — no panics
// tolerated; degrades to a decode error like every other surface.

import (
	oa "github.com/tubeof/proxyllm/internal/translate/openai"
	"testing"
)

func FuzzOpenAIChatDecodeRequest(f *testing.F) {
	f.Add(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	f.Add(`{"model":"m","messages":[{"role":"tool","tool_call_id":"c1","content":"ok"}],"tools":[{"type":"function","function":{"name":"f"}}]}`)
	f.Add(`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x.test/a.png"}}]}]}`)
	f.Add(`{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"q\"}"}}]}]}`)
	f.Add("not json")
	f.Fuzz(func(t *testing.T, body string) {
		_, _ = oa.Codec{}.DecodeRequest([]byte(body))
	})
}
