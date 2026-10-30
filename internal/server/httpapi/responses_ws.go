package httpapi

// GET /v1/responses with Upgrade: websocket — the OpenAI Responses
// WebSocket transport Codex CLI negotiates for wire_api = "responses".
//
// The connection carries a sequence of response.create frames. Each frame is
// decoded through the same Responses codec and pipeline as the HTTP surface;
// the only difference is the sink: one JSON text frame per event instead of
// SSE frames. A previous_response_id is resolved from a bounded per-connection
// history, so follow-up turns see the earlier input and assistant output.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/tubeof/proxyllm/internal/auth"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/openai"
)

const (
	wsWriteTimeout = 15 * time.Second
	// wsHistoryLimit bounds remembered responses per connection. Codex keeps
	// one conversation per connection; a small LRU is enough and keeps a
	// long-lived socket from growing without bound.
	wsHistoryLimit = 16
)

// handleResponsesWS serves the Responses WebSocket upgrade. A plain GET keeps
// the HTTP method contract (405, Allow: POST).
func (a *api) handleResponsesWS(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Auth is the client-key header on the handshake; Codex is not a
		// browser, so the Origin check adds nothing and would reject
		// legitimate CLI origins.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	conn.SetReadLimit(32 << 20)

	wc := &wsConn{c: conn}
	hist := newWSHistory(wsHistoryLimit)

	readCtx, readCancel := context.WithCancel(r.Context())
	defer readCancel()

	frames := make(chan []byte)
	go func() {
		defer close(frames)
		for {
			typ, data, err := conn.Read(readCtx)
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			select {
			case frames <- data:
			case <-readCtx.Done():
				return
			}
		}
	}()

	// One response at a time. A new create cancels the previous turn and
	// WAITS for its goroutine to finish before starting, so two responses
	// can never interleave frames on the same connection.
	var activeCancel context.CancelFunc
	var activeDone chan struct{}
	stopActive := func() {
		if activeCancel != nil {
			activeCancel()
		}
		if activeDone != nil {
			<-activeDone
		}
		activeCancel, activeDone = nil, nil
	}

	for data := range frames {
		var head struct {
			Type               string `json:"type"`
			PreviousResponseID string `json:"previous_response_id"`
		}
		if err := json.Unmarshal(data, &head); err != nil {
			_ = writeWSError(wc, errTypeInvalidRequest, "invalid websocket frame")
			continue
		}
		switch head.Type {
		case "response.create", "":
			stopActive()
			ctx, cancel := context.WithCancel(r.Context())
			done := make(chan struct{})
			activeCancel, activeDone = cancel, done
			go func(raw []byte, previousID string) {
				defer func() {
					cancel()
					close(done)
				}()
				a.serveWSResponse(ctx, wc, r, raw, previousID, hist)
			}(data, head.PreviousResponseID)
		case "response.cancel":
			if activeCancel != nil {
				activeCancel()
			}
		default:
			_ = writeWSError(wc, errTypeInvalidRequest, "unsupported event type "+head.Type)
		}
	}
	stopActive()
}

// serveWSResponse runs one response.create frame through the same pipeline as
// POST /v1/responses.
func (a *api) serveWSResponse(ctx context.Context, wc *wsConn, r *http.Request, raw []byte, previousID string, hist *wsHistory) {
	req, err := openai.ResponsesCodec{}.DecodeRequest(raw)
	if err != nil {
		msg := redactUpstreamMessage(err.Error())
		_ = writeWSError(wc, errTypeInvalidRequest, msg)
		a.writeRequestLog(ctx, auth.ClientKeyIDFromContext(ctx), "", "",
			http.StatusBadRequest, 0, nil, nil, msg)
		return
	}
	// WebSocket is streaming by definition; the frame may omit `stream`.
	req.Stream = true

	r = r.WithContext(ctx)
	r = a.beginRequest(r, protocolOpenAIResponses, raw, req)
	ctx = r.Context()
	keyID := auth.ClientKeyIDFromContext(ctx)

	if previousID != "" {
		prior, ok := hist.get(previousID)
		if !ok {
			msg := "unknown previous_response_id"
			_ = writeWSError(wc, errTypeNotFound, msg)
			a.writeRequestLog(ctx, auth.ClientKeyIDFromContext(ctx), req.Model, "",
				http.StatusNotFound, 0, nil, nil, msg)
			return
		}
		req.Messages = append(prior, req.Messages...)
		// The history is inlined locally. Never forward the proxy-minted id
		// to an upstream that never issued it (a Responses backend answers
		// 400 "previous response not found").
		req.PreviousResponseID = ""
	}

	compat := a.compatFor(ctx, req.Model)
	req, compatTrace, err := routing.ApplyCompat(compat, req)
	if err != nil {
		_, msg := a.writeRouteErrorWS(wc, err)
		a.writeRequestLog(ctx, keyID, req.Model, "", http.StatusBadRequest, 0,
			nil, routeTrace(compatTrace), msg)
		return
	}

	chain, routerTrace, err := a.router.ResolveChainFor(req.Model,
		requiredCapabilities(req), routing.WithKeyID(keyID), routing.WithRequestContext(ctx))
	if err != nil {
		status, msg := a.writeRouteErrorWS(wc, err)
		a.writeRequestLog(ctx, keyID, req.Model, "", status, 0,
			nil, routeTrace(compatTrace, routerTrace), msg)
		return
	}
	resolveTrace := &routing.Trace{}
	chain = a.resolveChainWireModels(ctx, chain, resolveTrace)

	start := time.Now()
	ch, cancel, execTrace, err := a.executor.ExecuteStream(ctx, chain, req)
	if err != nil {
		status, msg := a.writeRouteErrorWS(wc, err)
		a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace),
			status, time.Since(start).Milliseconds(), nil,
			routeTrace(compatTrace, routerTrace, resolveTrace, execTrace), msg)
		return
	}
	defer cancel()

	id := newResponseID()
	enc := openai.NewResponsesStreamEncoderWithID(wc, id)
	parts, text, terminalUsage, streamErr := relayWSStream(ctx, ch, enc, req.Model)

	// Remember the full turn for the next previous_response_id.
	hist.put(id, append(append([]ir.Message{}, req.Messages...),
		ir.Message{Role: ir.RoleAssistant, Parts: parts}))
	captureResponseText(ctx, req.Model, text)

	var errMsg string
	if streamErr != nil {
		errMsg = redactUpstreamMessage(streamErr.Message)
	}
	status := http.StatusOK
	if gone, abandoned := abandonmentStatus(ctx); abandoned {
		status = gone
	} else {
		a.executor.ReportStreamOutcome(lastProvider(execTrace), streamErr)
	}
	a.writeRequestLog(ctx, keyID, req.Model, lastProvider(execTrace),
		status, time.Since(start).Milliseconds(), terminalUsage,
		routeTrace(compatTrace, routerTrace, resolveTrace, execTrace), errMsg)
}

// writeRouteErrorWS maps a pipeline error onto a Responses error frame and
// returns the HTTP status for the request-log row.
func (a *api) writeRouteErrorWS(wc *wsConn, err error) (int, string) {
	status, errType, msg := mapRouteError(err)
	_ = writeWSError(wc, errType, msg)
	return status, msg
}

// writeWSError writes one flat Responses error event as a text frame.
func writeWSError(wc *wsConn, code, msg string) error {
	payload, err := json.Marshal(map[string]string{
		"type":    "error",
		"code":    code,
		"message": msg,
	})
	if err != nil {
		return err
	}
	return wc.Event("error", string(payload))
}

// relayWSStream forwards every canonical event to the encoder while
// collecting the assistant parts and text for the next turn.
func relayWSStream(ctx context.Context, ch <-chan ir.StreamEvent, enc openai.StreamEncoderWithID, clientModel string) ([]ir.Part, string, *ir.Usage, *ir.UpstreamError) {
	type openPart struct {
		kind string
		id   string
		name string
		buf  strings.Builder
	}
	opened := make(map[int]*openPart)
	closed := make(map[int]ir.Part)
	var text strings.Builder
	var usage *ir.Usage
	var streamErr *ir.UpstreamError

	for {
		select {
		case <-ctx.Done():
			return collectParts(closed), text.String(), usage, streamErr
		case ev, ok := <-ch:
			if !ok {
				return collectParts(closed), text.String(), usage, streamErr
			}
			if ev.Type == ir.Start {
				if ev.Response == nil {
					ev.Response = &ir.Response{}
				}
				ev.Response.Model = clientModel
			}
			switch ev.Type {
			case ir.BlockStart:
				p := &openPart{}
				switch v := ev.Part.(type) {
				case ir.TextPart:
					p.kind = "text"
				case ir.ThinkingPart:
					p.kind = "thinking"
				case ir.ToolCallPart:
					p.kind, p.id, p.name = "tool", v.ID, v.Name
				case ir.ServerToolUsePart:
					p.kind, p.id, p.name = "server_tool", v.ID, v.Name
				default:
					p.kind = "other"
				}
				opened[ev.Index] = p
			case ir.Delta:
				if p := opened[ev.Index]; p != nil {
					switch ev.Delta.Kind {
					case ir.DeltaText:
						p.buf.WriteString(ev.Delta.Text)
						text.WriteString(ev.Delta.Text)
					case ir.DeltaThinking, ir.DeltaSignature:
						p.buf.WriteString(ev.Delta.Text)
					case ir.DeltaToolInput:
						p.buf.WriteString(ev.Delta.PartialJSON)
					}
				}
			case ir.BlockStop:
				if p := opened[ev.Index]; p != nil {
					switch p.kind {
					case "text":
						closed[ev.Index] = ir.TextPart{Text: p.buf.String()}
					case "thinking":
						closed[ev.Index] = ir.ThinkingPart{Thinking: p.buf.String()}
					case "tool":
						closed[ev.Index] = ir.ToolCallPart{
							ID: p.id, Name: p.name, Input: validToolInput(p.buf.String()),
						}
					case "server_tool":
						closed[ev.Index] = ir.ServerToolUsePart{
							ID: p.id, Name: p.name, Input: validToolInput(p.buf.String()),
						}
					}
					delete(opened, ev.Index)
				}
			case ir.MessageDelta:
				if ev.MessageDelta != nil {
					u := ev.MessageDelta.Usage
					usage = &u
				}
			case ir.Error:
				if ev.Error != nil {
					streamErr = ev.Error
				}
				_ = enc.Write(ctx, ev)
				return collectParts(closed), text.String(), usage, streamErr
			}
			if err := enc.Write(ctx, ev); err != nil {
				return collectParts(closed), text.String(), usage, streamErr
			}
		}
	}
}

func validToolInput(raw string) json.RawMessage {
	if raw == "" || !json.Valid([]byte(raw)) {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(raw)
}

func collectParts(closed map[int]ir.Part) []ir.Part {
	if len(closed) == 0 {
		return nil
	}
	indices := make([]int, 0, len(closed))
	for i := range closed {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	out := make([]ir.Part, 0, len(indices))
	for _, i := range indices {
		out = append(out, closed[i])
	}
	return out
}

// wsConn adapts the websocket to openai.EventSink. Writes are serialized
// because a response goroutine and a cancel path may overlap.
type wsConn struct {
	c  *websocket.Conn
	mu sync.Mutex
}

func (w *wsConn) Event(_ string, data string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), wsWriteTimeout)
	defer cancel()
	return w.c.Write(ctx, websocket.MessageText, []byte(data))
}

// Comment has no WebSocket equivalent; comments are SSE-only.
func (w *wsConn) Comment(string) error { return nil }

// wsHistory is a bounded per-connection response history.
type wsHistory struct {
	mu    sync.Mutex
	max   int
	order []string
	turns map[string][]ir.Message
}

func newWSHistory(max int) *wsHistory {
	return &wsHistory{max: max, turns: make(map[string][]ir.Message)}
}

func (h *wsHistory) get(id string) ([]ir.Message, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	msgs, ok := h.turns[id]
	if !ok {
		return nil, false
	}
	return cloneMessages(msgs), true
}

func (h *wsHistory) put(id string, msgs []ir.Message) {
	if id == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.turns[id]; !exists {
		h.order = append(h.order, id)
		if len(h.order) > h.max {
			delete(h.turns, h.order[0])
			h.order = h.order[1:]
		}
	}
	h.turns[id] = cloneMessages(msgs)
}

// cloneMessages copies each Parts slice: ApplyCompat's stripThinkingBlocks
// compacts msg.Parts in place, which would otherwise mutate a stored turn
// when a later request branches from an older response id.
func cloneMessages(msgs []ir.Message) []ir.Message {
	out := make([]ir.Message, len(msgs))
	for i, m := range msgs {
		out[i] = ir.Message{Role: m.Role, Parts: append([]ir.Part(nil), m.Parts...)}
	}
	return out
}

func newResponseID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("resp_%d", time.Now().UnixNano())
	}
	return "resp_" + hex.EncodeToString(b[:])
}
