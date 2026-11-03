package openai_test

// Fuzz the Responses decoder stream: no panic, no unbounded crash on
// arbitrary input. The seed corpus is not hand-picked; `go test` derives it
// from the test's own calls (f.Add).

import (
	"testing"

	oa "github.com/tubeof/proxyllm/internal/translate/openai"
)

func FuzzResponsesDecodeRequest(f *testing.F) {
	seeds := []string{
		`{"model":"gpt-test","input":"hi"}`,
		`{"model":"gpt-test","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
		`{"model":"gpt-test","input":[{"type":"web_search_call","id":"ws_9","status":"completed","action":{"type":"search","query":"q"}}]}`,
		`{"model":"gpt-test","input":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}],"tools":[]}`,
		`{"model":"gpt-test","input":"hi","store":true,"previous_response_id":"resp_1"}`,
		`{"model":"gpt-test","input":"hi","tools":[{"type":"web_search"}]}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, body string) {
		_, _ = oa.ResponsesCodec{}.DecodeRequest([]byte(body))
	})
}
