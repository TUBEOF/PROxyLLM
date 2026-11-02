package anthropic_test

// WP8: no panic on arbitrary wire bytes; the decoder must degrade to an
// error or partial events, never crash (the SSE lines cap at 8 MiB).

import (
	"context"
	"strings"
	"testing"

	anthropic "github.com/tubeof/proxyllm/internal/translate/anthropic"
)

func FuzzAnthropicStreamDecode(f *testing.F) {
	seeds := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"a\\\":1\"}}\n\n",
		"event: error\ndata: {\"type\":\"error\"}\n\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, wire string) {
		dec := anthropic.NewStreamDecoder(strings.NewReader(wire))
		for i := 0; i < 4096; i++ {
			if _, err := dec.Read(context.Background()); err != nil {
				break
			}
		}
	})
}
