// Package translate defines the codec contracts every protocol implements
// against the canonical IR. Concrete codecs live in
// subpackages (openai, anthropic, ollama); this package owns the shared
// interfaces and cross-codec test helpers.
package translate

import (
	"context"

	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// RequestDecoder decodes a protocol wire request body into the IR.
type RequestDecoder interface {
	DecodeRequest(raw []byte) (ir.Request, error)
}

// ResponseEncoder encodes an IR response into protocol wire bytes.
type ResponseEncoder interface {
	EncodeResponse(resp ir.Response) ([]byte, error)
}

// ResponseDecoder decodes a protocol wire response body into the IR
// (used by provider clients reading upstream responses).
type ResponseDecoder interface {
	DecodeResponse(raw []byte) (ir.Response, error)
}

// RequestEncoder encodes an IR request into protocol wire bytes (used by
// provider clients writing upstream requests).
type RequestEncoder interface {
	EncodeRequest(req ir.Request) ([]byte, error)
}

// StreamDecoder turns a protocol wire byte stream into the canonical event
// sequence. Implementations are pushdown state machines: they own
// tool-call/JSON-fragment assembly state and enforce ordering.
//
// Events must be emitted promptly as they arrive; a decoder never holds an
// event back to batch it. Read blocks until the next event, returns io.EOF
// at clean stream end, and returns a terminal ir.Error event before EOF on
// upstream failure.
type StreamDecoder interface {
	// Read returns the next canonical event from the wire stream.
	Read(ctx context.Context) (ir.StreamEvent, error)
}

// StreamEncoder writes canonical events to a client connection in protocol
// wire form, flushing per event so bytes reach the client as they arrive.
// The SSE writer also emits a spec-ignored comment right after the header,
// which releases the HTTP/2 HEADERS frame a proxy would otherwise hold back.
type StreamEncoder interface {
	// Write emits one event and flushes.
	Write(ctx context.Context, ev ir.StreamEvent) error
}
