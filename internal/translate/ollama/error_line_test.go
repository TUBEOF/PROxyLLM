package ollama

// Error events must surface on the NDJSON wire as {"error":..., "done":true};
// silently ending the stream hid an upstream failure from the client.

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/translate/ir"
)

func TestStreamDecoder_ErrorLine(t *testing.T) {
	d := NewStreamDecoder(strings.NewReader(`{"error":"boom"}` + "\n"))
	var events []ir.StreamEvent
	for {
		ev, err := d.Read(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		events = append(events, ev)
	}
	for _, ev := range events {
		if ev.Type == ir.Error && ev.Error != nil && strings.Contains(ev.Error.Message, "boom") {
			return
		}
	}
	t.Fatalf("events = %+v, want an Error event with boom", events)
}

func TestStreamEncoder_ErrorLine(t *testing.T) {
	var buf bytes.Buffer
	enc := NewStreamEncoder(&buf)
	err := enc.Write(context.Background(), ir.StreamEvent{
		Type:  ir.Error,
		Error: &ir.UpstreamError{Message: "boom"},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `"error":"boom"`) || !strings.Contains(out, `"done":true`) {
		t.Fatalf("error line = %q, want error + done", out)
	}
}
