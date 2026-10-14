// Package httpx is the shared HTTP transport foundation for the concrete
// providers (plan §4.1): request encode → POST → two-tier error mapping
// (FO-2) → stream pump. Custom OpenAI-compatible and Ollama Cloud differ
// only in codec, path, and error-body shape — the transport semantics
// (Bearer auth, Retry-After parsing, cancellation, bounded-channel
// backpressure, terminal mid-stream error events) are protocol facts every
// HTTP provider shares.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/translate"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// streamChanBuffer bounds the canonical event channel (plan §4.4
// backpressure): the pump pauses reading upstream when the consumer is
// slow instead of buffering the whole stream in memory.
const streamChanBuffer = 16

// Client is one provider's HTTP transport configuration.
type Client struct {
	// BaseURL is the API root, e.g. "https://api.example.com/v1". ChatPath
	// is appended to it.
	BaseURL string

	// ChatPath is the chat endpoint relative to BaseURL (e.g.
	// "/chat/completions" for OpenAI-compatible, "/api/chat" for Ollama
	// Cloud).
	ChatPath string

	// APIKey rides Authorization: Bearer (O1/L2).
	APIKey string

	// ErrorShape selects how non-2xx bodies decode into UpstreamError
	// fields.
	ErrorShape ErrorShape

	HTTP *http.Client

	// HeaderHook optionally supplies per-request headers applied over the
	// defaults — for providers whose credentials rotate between dispatches
	// (e.g. OAuth access tokens refreshed lazily on demand). stream reports
	// whether the request is a streaming request (Accept negotiation).
	HeaderHook func(ctx context.Context, stream bool) (http.Header, error)
}

// ErrorShape names the upstream's non-2xx body layout.
type ErrorShape string

const (
	// ErrorOpenAI is `{"error":{"message":..,"type":..,"code":..}}` (O10).
	ErrorOpenAI ErrorShape = "openai"

	// ErrorOllama is `{"error":"message"}` — plain string, no typed codes.
	ErrorOllama ErrorShape = "ollama"
)

// EncodeRequest is the codec-side request encoder every provider uses.
type EncodeRequest = func(req ir.Request) ([]byte, error)

// Complete performs one non-streaming round trip: encode → POST → map a
// non-2xx into *provider.UpstreamError (FO-2) → decode the 2xx body.
// Transport failures (no response at all) are returned as-is — routing
// classifies them as early-stream retryable via the zero-status path.
func (c *Client) Complete(
	ctx context.Context,
	req ir.Request,
	encode EncodeRequest,
	decode func(raw []byte) (ir.Response, error),
) (ir.Response, error) {
	raw, err := encode(req)
	if err != nil {
		return ir.Response{}, fmt.Errorf("httpx: encode request: %w", err)
	}
	body, err := c.post(ctx, req.Stream, raw)
	if err != nil {
		return ir.Response{}, err
	}
	resp, err := decode(body)
	if err != nil {
		return ir.Response{}, fmt.Errorf("httpx: decode response: %w", err)
	}
	return resp, nil
}

// Stream starts one streaming round trip and pumps canonical events into a
// bounded channel (plan §4.4). The returned error covers dispatch
// failures only (FO-1); failures after the first event are terminal
// ir.Error events on the channel. cancel aborts the upstream request and
// releases its resources (C2).
func (c *Client) Stream(
	ctx context.Context,
	req ir.Request,
	encode EncodeRequest,
	newDecoder func(r io.Reader) translate.StreamDecoder,
) (<-chan ir.StreamEvent, func(), error) {
	streamReq := req
	streamReq.Stream = true
	raw, err := encode(streamReq)
	if err != nil {
		return nil, nil, fmt.Errorf("httpx: encode request: %w", err)
	}

	// The request carries its own cancellable context: the idle watchdog has to
	// be able to break a read that is already in flight, and only cancelling the
	// context the request was issued with reaches the transport's reader. The
	// pump's own context is a child of it, so one cancel covers both.
	reqCtx, reqCancel := context.WithCancel(ctx)
	httpReq, err := c.newRequest(reqCtx, true, raw)
	if err != nil {
		reqCancel()
		return nil, nil, err
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		reqCancel()
		return nil, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		up := c.mapError(resp)
		_ = resp.Body.Close()
		reqCancel()
		return nil, nil, up
	}

	events := make(chan ir.StreamEvent, streamChanBuffer)
	pumpCtx, pumpCancel := context.WithCancel(reqCtx)
	// cancel aborts the upstream request and closes its body (C2). It is
	// idempotent — the pump's own exit path also closes resp.Body.
	cancel := func() {
		reqCancel()
		pumpCancel()
		_ = resp.Body.Close()
	}
	go c.pump(pumpCtx, &stallReader{body: resp.Body, cancel: cancel}, newDecoder, events)
	return events, cancel, nil
}

// stallIdleTimeout bounds how long a stream may deliver nothing at all.
//
// Without it a provider that accepts the request and then goes quiet holds the
// client until the client's own watchdog gives up — the connection is open, the
// body is open, and nobody is waiting on anything. The deadline is measured on
// raw upstream bytes rather than on translated events: a reasoning model can
// legitimately produce no visible output for long stretches while framing bytes
// keep arriving, and treating that as a stall would cut off exactly the answers
// that take longest to compose.
// Variable rather than constant so a test can shorten it: a five-minute wait is
// not something a unit test can sit through, and the behaviour under test is the
// reset, not the number.
var stallIdleTimeout = 5 * time.Minute

// stallReader resets an idle timer on every byte read from the upstream body and
// cancels the request when it expires. It sits below the decoder, so it sees what
// the wire is doing rather than what the protocol makes of it.
type stallReader struct {
	body   io.ReadCloser
	cancel func()
	timer  *time.Timer
	once   sync.Once

	mu      sync.Mutex
	stalled bool
}

// Stalled reports whether the idle watchdog cut this stream short, as opposed to
// the consumer walking away. The pump has to tell the two apart: one is a failure
// the client must be told about, the other is a client that is no longer there.
func (r *stallReader) Stalled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stalled
}

func (r *stallReader) fire() {
	r.mu.Lock()
	r.stalled = true
	r.mu.Unlock()
	r.cancel()
}

func (r *stallReader) Read(p []byte) (int, error) {
	r.start()
	n, err := r.body.Read(p)
	if n > 0 {
		r.reset()
	}
	return n, err
}

func (r *stallReader) Close() error {
	r.stop()
	return r.body.Close()
}

func (r *stallReader) start() {
	r.once.Do(func() {
		r.timer = time.AfterFunc(stallIdleTimeout, r.fire)
	})
}

func (r *stallReader) reset() {
	if r.timer != nil {
		r.timer.Reset(stallIdleTimeout)
	}
}

func (r *stallReader) stop() {
	if r.timer != nil {
		r.timer.Stop()
	}
}

// pump reads wire events from the decoder and forwards canonical events.
// Terminal upstream failures become an ir.Error event followed by channel
// close (FO-1: mid-stream failures are never retried); cancellation closes
// the channel without an error event (C2: client disconnect is not a
// failure the consumer needs to see).
func (c *Client) pump(
	ctx context.Context,
	body *stallReader,
	newDecoder func(r io.Reader) translate.StreamDecoder,
	events chan<- ir.StreamEvent,
) {
	defer close(events)
	defer func() { _ = body.Close() }()
	dec := newDecoder(body)
	terminated := false
	for {
		ev, err := dec.Read(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				if !terminated && ctx.Err() == nil {
					// A stream that stops without its terminal marker is not the
					// same thing as one that ended. The client is holding a
					// partial answer and waiting for an event that is never
					// coming, so closing the channel quietly would leave it
					// waiting on a body that is already over. Say what happened
					// and let the surface render a conform error.
					select {
					case events <- ir.StreamEvent{Type: ir.Error, Error: &ir.UpstreamError{
						Message: "upstream closed the stream without a terminal event",
					}}:
					case <-ctx.Done():
					}
				}
				return
			}
			if ctx.Err() != nil {
				if body.Stalled() {
					// The watchdog cut a silent upstream short. The client is
					// still there and is owed the reason, not a closed channel.
					select {
					case events <- ir.StreamEvent{Type: ir.Error, Error: &ir.UpstreamError{
						Message: "upstream stopped sending without a terminal event",
					}}:
					case <-time.After(5 * time.Second):
					}
				}
				return // otherwise: canceled by the consumer — clean exit
			}
			select {
			case events <- ir.StreamEvent{Type: ir.Error, Error: &ir.UpstreamError{Message: err.Error()}}:
			case <-ctx.Done():
			}
			return
		}
		if ev.Type == ir.Done || ev.Type == ir.Error {
			terminated = true
		}
		select {
		case events <- ev:
		case <-ctx.Done():
			return
		}
	}
}

// post sends one request and returns the 2xx body. Non-2xx responses map
// to *provider.UpstreamError; transport failures return unwrapped.
func (c *Client) post(ctx context.Context, stream bool, raw []byte) ([]byte, error) {
	httpReq, err := c.newRequest(ctx, stream, raw)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, err // transport failure: no UpstreamError (FO-2 tier 1)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, c.mapError(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("httpx: read response body: %w", err)
	}
	return body, nil
}

// Get performs one authenticated GET round trip against a path relative to
// BaseURL (ADR-010 model discovery): Bearer auth + the provider's error
// shape for non-2xx, the 2xx body as raw bytes. Transport failures return
// as-is.
func (c *Client) Get(ctx context.Context, path string) ([]byte, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("httpx: build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpReq.Header.Set("Accept", "application/json")
	if c.HeaderHook != nil {
		extra, err := c.HeaderHook(ctx, false)
		if err != nil {
			return nil, fmt.Errorf("httpx: build provider headers: %w", err)
		}
		for name, vals := range extra {
			httpReq.Header[name] = vals
		}
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, err // transport failure: no UpstreamError (FO-2 tier 1)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, c.mapError(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("httpx: read response body: %w", err)
	}
	return body, nil
}

// newRequest builds the POST request with Bearer auth (O1/L2). The
// HeaderHook, when present, overlays per-request headers — used by
// providers whose credentials rotate between dispatches (codex OAuth) and
// for Accept negotiation on streaming requests (X23).
func (c *Client) newRequest(ctx context.Context, stream bool, raw []byte) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+c.ChatPath, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("httpx: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpReq.Header.Set("Accept", "application/json")
	if c.HeaderHook != nil {
		extra, err := c.HeaderHook(ctx, stream)
		if err != nil {
			return nil, fmt.Errorf("httpx: build provider headers: %w", err)
		}
		for name, vals := range extra {
			httpReq.Header[name] = vals
		}
	}
	return httpReq, nil
}

// mapError converts a non-2xx response into the typed upstream error with
// both classification tiers: HTTP status (tier 1) and the decoded body
// error object (tier 2), plus the Retry-After hint (O10).
func (c *Client) mapError(resp *http.Response) *provider.UpstreamError {
	up := &provider.UpstreamError{HTTPStatus: resp.StatusCode}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
			up.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return up
	}
	switch c.ErrorShape {
	case ErrorOllama:
		var w struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &w) == nil && w.Error != "" {
			up.Message = w.Error
		}
	default: // ErrorOpenAI
		var w struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    any    `json:"code"` // string or null in the wild (O10)
			} `json:"error"`
		}
		if json.Unmarshal(raw, &w) == nil {
			up.Message = w.Error.Message
			up.Type = w.Error.Type
			if s, ok := w.Error.Code.(string); ok {
				up.Code = s
			}
		}
	}
	if up.Message == "" && len(raw) > 0 {
		// Undecodable body: keep the raw text (redaction runs at the log
		// layer; credentials never ride errors because headers are not
		// copied).
		up.Message = string(bytes.TrimSpace(raw))
	}
	return up
}
