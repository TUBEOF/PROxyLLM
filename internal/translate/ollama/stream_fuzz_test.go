package ollama_test

// WP8: no panic on arbitrary NDJSON wire bytes; the decoder degrades to an
// error, never crashes (the NDJSON line cap is 8 MiB).

import (
	"context"
	"strings"
	"testing"

	ollama "github.com/tubeof/proxyllm/internal/translate/ollama"
)

func FuzzOllamaStreamDecode(f *testing.F) {
	seeds := []string{
		`{"model":"m","message":{"role":"assistant","content":"hi"},"done":false}` + "\n",
		`{"model":"m","message":{"content":"done"},"done":true}` + "\n",
		"garbage\n",
		"{\"model\":\"m\",\"message\":{\"tool_calls\":[{\"function\":{\"name\":\"f\",\"arguments\":{\"a\":1}}}]},\"done\":true}\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, wire string) {
		dec := ollama.NewStreamDecoder(strings.NewReader(wire + "\n"))
		for i := 0; i < 4096; i++ {
			if _, err := dec.Read(context.Background()); err != nil {
				break
			}
		}
	})
}
