package webui

// GET /api/admin/logs/stream (Logs-page redesign, 2026-10-06): the admin
// log SSE. Wire: `event: hello` (ack) → `event: running` (the in-flight
// snapshot; every snapshot REPLACES the previous list) → `event: log` rows
// (new ones via the store cursor, resumable client-side) → 15 s `: ping`
// heartbeats. GET is immune to the CSRF layer (session cookie suffices —
// native EventSource works without headers); the connect keeps the admin
// session warm (TouchSession on the middleware path) — documented side
// effect in docs/api-reference.
//
// Backpressure mirrors internal/translate/sse doctrine: a slow/dead client
// is dropped on the first failed flush — it never becomes the proxy's
// backpressure; EventSource auto-reconnect restores state.

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/tubeof/proxyllm/internal/reqlog"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/translate/sse"
)

const (
	logStreamHeartbeatDefault = 15 * time.Second
	logStreamBackfill         = 200 // rows per cursor drain batch
)

// streamLogs serves the live log SSE stream.
func (a *AdminAPI) streamLogs(w http.ResponseWriter, r *http.Request) {
	if a.stream == nil {
		writeError(w, http.StatusServiceUnavailable, "live log stream not wired")
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	swr := sse.NewWriter(w)
	swr.WriteHeader(http.StatusOK)

	events, cancel := a.stream.Subscribe(r.Context())
	defer cancel()

	// hello + initial running snapshot; the page fetch backfills history,
	// the stream only carries the tail (new rows via the cursor below).
	if err := swr.Event("hello", `{"ok":true}`); err != nil {
		return
	}
	if err := a.sendRunning(swr, events); err != nil {
		return
	}

	// Client cursor: the newest row id seen; wakes drain the store tail.
	cursor := int64(0)
	if latest, err := a.store.ListRequestLogsAfter(r.Context(), storage.RequestLogFilter{}, 1, 0); err == nil && len(latest) > 0 {
		cursor = latest[0].ID // existing rows arrive via the page fetch
	}

	heartbeat := a.streamHeartbeat
	if heartbeat <= 0 {
		heartbeat = logStreamHeartbeatDefault
	}
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if err := swr.Comment("ping"); err != nil {
				return
			}
			fl.Flush()
		case <-a.stream.Wait():
			// Drain the store tail first (durable rows — loss-tolerant:
			// a missed wake only delays them).
			for {
				rows, err := a.store.ListRequestLogsAfter(r.Context(), storage.RequestLogFilter{}, logStreamBackfill, cursor)
				if err != nil || len(rows) == 0 {
					break
				}
				for _, row := range rows {
					if row.ID > cursor {
						cursor = row.ID
					}
					if err := sendLog(swr, reqlog.LogRow{
						ID: row.ID, Timestamp: row.Timestamp, ClientKeyID: row.ClientKeyID,
						ProviderID: row.ProviderID, Model: row.Model, Status: row.Status,
						LatencyMS: row.LatencyMS, Usage: row.Usage, RouteTrace: row.RouteTrace,
						ConversationID: row.ConversationID, Protocol: row.Protocol,
						Features: row.Features, ErrorRedacted: row.ErrorRedacted,
					}); err != nil {
						return
					}
				}
				if len(rows) < logStreamBackfill {
					break
				}
			}
			fl.Flush()
			// Hub events (running snapshots + finished rows published since
			// the last wake).
			if err := a.drainHub(swr, events); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// drainHub pipes buffered hub events onto the wire (non-blocking — the
// Subscribe channel keeps the buffer; a full buffer means the hub will
// drop this subscriber per its own policy).
func (a *AdminAPI) drainHub(swr *sse.Writer, events <-chan reqlog.Event) error {
	for {
		select {
		case ev := <-events:
			if err := sendEvent(swr, ev); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

// sendRunning sends the CURRENT tracker snapshot (from a buffered running
// event — every snapshot replaces the previous list, so replaying the
// latest is equivalent to reading state).
func (a *AdminAPI) sendRunning(swr *sse.Writer, events <-chan reqlog.Event) error {
	var latest *reqlog.Event
	for {
		select {
		case ev := <-events:
			if ev.Kind == reqlog.EventRunning {
				e := ev
				latest = &e
			}
			continue
		default:
		}
		break
	}
	if latest == nil {
		return nil
	}
	return sendEvent(swr, *latest)
}

// sendEvent marshals one hub event onto the wire.
func sendEvent(swr *sse.Writer, ev reqlog.Event) error {
	var name string
	var payload any
	switch ev.Kind {
	case reqlog.EventRunning:
		name, payload = "running", ev.Running
	case reqlog.EventLog:
		name, payload = "log", ev.Row
	default:
		return nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil // one bad row never kills the stream
	}
	return swr.Event(name, string(raw))
}

// sendLog marshals and sends one `log` event.
func sendLog(swr *sse.Writer, row reqlog.LogRow) error {
	raw, err := json.Marshal(row)
	if err != nil {
		return nil
	}
	return swr.Event("log", string(raw))
}
