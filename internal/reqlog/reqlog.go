// Package reqlog is the admin log stream's backing store: an in-flight
// registry (the WebUI's running rows) plus a finished-row broadcast and
// SSE subscriber fan-out. It is transport-agnostic — no storage or webui
// imports; the SSE handler backfills history from the store cursor itself.
//
// Backpressure mirrors internal/translate/sse doctrine: every subscriber
// owns a bounded channel; a slow admin browser is DROPPED (channel closed,
// removed) rather than becoming the proxy's backpressure — EventSource
// auto-reconnect restores state via the hello/snapshot handshake.
package reqlog

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Inflight is one request between surface entry (Start) and its terminal
// log write (finish).
type Inflight struct {
	ID             string `json:"id"`         // "run_" + 6 hex of a process-local counter
	StartedAt      int64  `json:"started_at"` // unix seconds
	ClientKeyID    string `json:"client_key_id,omitempty"`
	Model          string `json:"model"`
	Protocol       string `json:"protocol,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"` // derived hint (final id resolves at the log write)
	ConvKey        string `json:"conv_key,omitempty"`        // internal grouping key for the WebUI's running-row chaining
}

// LogRow is one terminal request-log row (the same field set the admin log
// DTO projects; the caller builds it so reqlog stays transport-agnostic).
type LogRow struct {
	ID             int64           `json:"id"`
	Timestamp      int64           `json:"timestamp"`
	ClientKeyID    string          `json:"client_key_id"`
	ProviderID     string          `json:"provider_id"`
	Model          string          `json:"model"`
	Status         int             `json:"status"`
	LatencyMS      int64           `json:"latency_ms"`
	Usage          json.RawMessage `json:"usage,omitempty"`
	RouteTrace     json.RawMessage `json:"route_trace,omitempty"`
	ConversationID string          `json:"conversation_id,omitempty"`
	Protocol       string          `json:"protocol,omitempty"`
	Features       json.RawMessage `json:"features,omitempty"`
	ErrorRedacted  string          `json:"error_redacted,omitempty"`

	// Display projections, filled by the admin SSE handler (which owns the
	// store). They mirror the page DTO's fields of the same name so a row
	// arriving live renders exactly like the same row after the next page
	// fetch — without them a live row would show raw ids next to named
	// rows. reqlog itself stays storage-agnostic and leaves them empty.
	KeyName      string `json:"key_name,omitempty"`
	KeyPrefix    string `json:"key_prefix,omitempty"`
	ProviderName string `json:"provider_name,omitempty"`
	ProviderKind string `json:"provider_kind,omitempty"`
}

// EventKind discriminates Stream events.
type EventKind int

const (
	// EventRunning carries the FULL in-flight snapshot — every snapshot
	// replaces the subscriber's previous list (state, not delta).
	EventRunning EventKind = iota
	// EventLog carries one finished row (lossy under load: a subscriber
	// that cannot keep up is dropped instead of stalling the proxy).
	EventLog
)

// Event is one Stream broadcast.
type Event struct {
	Kind    EventKind  `json:"kind"`
	Running []Inflight `json:"running,omitempty"`
	Row     *LogRow    `json:"row,omitempty"`
}

const (
	subBuffer   = 16        // per-subscriber event buffer
	staleWindow = time.Hour // reap in-flight entries older than this
	notifyCap   = 1         // hub notify channel capacity
)

// subscriber is one SSE fan-out target. Its OWN mutex and closed flag make the
// send/close race free without the hub's lock (2026-10-10): the fan-out used to
// take the hub mutex once PER SUBSCRIBER PER EVENT, so every request's
// registration serialized behind the whole broadcast.
type subscriber struct {
	ch     chan Event
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	closed bool
}

// send delivers one event, evicting or dropping as the buffer demands. False
// means the subscriber is gone (it closed, or this send closed it).
func (sub *subscriber) send(ev Event) bool {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if sub.closed {
		return false
	}
	select {
	case sub.ch <- ev:
		return true
	default:
	}
	if ev.Kind == EventRunning {
		// Running is state, not a delta: drop the oldest queued snapshot.
		select {
		case <-sub.ch:
		default:
		}
		select {
		case sub.ch <- ev:
		default:
		}
		return true
	}
	// An EventLog with a full buffer means this subscriber cannot keep up. Close
	// it here, under its own lock, so a concurrent send can never reach a closed
	// channel.
	sub.closed = true
	close(sub.ch)
	return false
}

// close ends the registration from the outside (client disconnect). Idempotent
// with send's own close.
func (sub *subscriber) close() {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if sub.closed {
		return
	}
	sub.closed = true
	close(sub.ch)
}

// Stream is the in-flight registry + broadcast hub. Safe for concurrent
// use; run with the -race gate (streaming-adjacent doctrine).
type Stream struct {
	// Two locks, not one (2026-10-10): a request's registration must not queue
	// behind the broadcast, and reading the in-flight set must not queue behind
	// another reader.
	inflightMu sync.RWMutex
	inflight   map[string]Inflight

	subsMu sync.RWMutex
	subs   map[*subscriber]struct{}

	seq atomic.Uint64
	now func() time.Time // injectable clock for tests

	// notify wakes SSE handlers (cap 1, non-blocking send): a missed wake
	// only delays — handlers re-read their own cursor + snapshot on every
	// wake, so the wire is loss-tolerant.
	notify chan struct{}
}

// New constructs an empty Stream.
func New() *Stream {
	return &Stream{
		inflight: make(map[string]Inflight),
		subs:     make(map[*subscriber]struct{}),
		now:      time.Now,
		notify:   make(chan struct{}, notifyCap),
	}
}

// Wait exposes the hub notify channel: each publish makes one non-blocking
// send so at most one wake is pending — a missed wake only delays.
func (s *Stream) Wait() <-chan struct{} { return s.notify }

// InflightCount reports the current in-flight registration count (the
// no-leak tests' probe).
func (s *Stream) InflightCount() int {
	s.inflightMu.RLock()
	defer s.inflightMu.RUnlock()
	return len(s.inflight)
}

// SubCount reports the current subscriber registration count.
func (s *Stream) SubCount() int {
	s.subsMu.RLock()
	defer s.subsMu.RUnlock()
	return len(s.subs)
}

func (s *Stream) ping() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// Start registers an in-flight request and broadcasts a running snapshot.
// It returns the idempotent finish function (exactly-once per request;
// called from the log write) and the assigned run id.
func (s *Stream) Start(in Inflight) (finish func(row LogRow), runID string) {
	if in.ID == "" {
		in.ID = "run_" + hex6(s.seq.Add(1))
	}
	if in.StartedAt == 0 {
		in.StartedAt = s.now().Unix()
	}
	runID = in.ID

	var once sync.Once
	finish = func(row LogRow) {
		once.Do(func() {
			s.inflightMu.Lock()
			delete(s.inflight, in.ID)
			inflight := s.snapshot()
			s.inflightMu.Unlock()
			subs := s.subList()
			// Running first (removes the pseudo-row), then the terminal row.
			s.publish(subs, Event{Kind: EventRunning, Running: inflight})
			s.publish(subs, Event{Kind: EventLog, Row: &row})
			s.ping()
		})
	}

	s.inflightMu.Lock()
	s.reapStale()
	s.inflight[in.ID] = in
	inflight := s.snapshot()
	s.inflightMu.Unlock()
	subs := s.subList()
	s.publish(subs, Event{Kind: EventRunning, Running: inflight})
	s.ping()
	return finish, runID
}

// PublishRow broadcasts one terminal row as a plain log event (no tracker
// change): rows that never registered an in-flight entry (e.g. the
// count_tokens pre-decode path) stay observable on the live stream.
func (s *Stream) PublishRow(row LogRow) {
	s.publish(s.subList(), Event{Kind: EventLog, Row: &row})
	s.ping()
}

// Subscribe registers a new subscriber: its FIRST received event is the
// current running snapshot (buffered directly — no broadcast wave; the
// caller is responsible for store backfill). The returned cancel (and ctx
// cancellation) closes the channel and frees the registration — no
// goroutine leak on client disconnect.
func (s *Stream) Subscribe(ctx context.Context) (<-chan Event, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	sub := &subscriber{ch: make(chan Event, subBuffer), ctx: ctx, cancel: cancel}

	s.inflightMu.RLock()
	inflight := s.snapshot()
	s.inflightMu.RUnlock()

	s.subsMu.Lock()
	s.subs[sub] = struct{}{}
	s.subsMu.Unlock()

	// The first event is the current snapshot, buffered directly: the buffer is
	// untouched, so this send always lands, and it goes through the same
	// per-subscriber lock as every later one.
	sub.send(Event{Kind: EventRunning, Running: inflight})

	// One goroutine per subscriber; exits on ctx cancel (client disconnect or
	// the returned cancel) and removes + closes the registration. The close goes
	// through the subscriber's own lock, so it cannot race a concurrent send.
	go func() {
		<-ctx.Done()
		s.subsMu.Lock()
		delete(s.subs, sub)
		s.subsMu.Unlock()
		sub.close()
	}()
	return sub.ch, cancel
}

// reapStale drops in-flight entries older than staleWindow (a request that
// died without reaching finish — only possible on a panic path — self-heals
// out of the running view). Call with mu held.
func (s *Stream) reapStale() {
	cutoff := s.now().Add(-staleWindow).Unix()
	for id, in := range s.inflight {
		if in.StartedAt < cutoff {
			delete(s.inflight, id)
		}
	}
}

// snapshot copies the in-flight list in stable id order. Call with mu held.
func (s *Stream) snapshot() []Inflight {
	out := make([]Inflight, 0, len(s.inflight))
	for _, in := range s.inflight {
		out = append(out, in)
	}
	return out
}

// subList copies the subscriber set. Call with mu held.
// subList snapshots the subscriber set under its reader lock.
func (s *Stream) subList() []*subscriber {
	s.subsMu.RLock()
	defer s.subsMu.RUnlock()
	subs := make([]*subscriber, 0, len(s.subs))
	for sub := range s.subs {
		subs = append(subs, sub)
	}
	return subs
}

// publish fan-outs one event: running snapshots replace (drop the oldest
// buffered snapshot to make room — idempotent state), log rows drop the
// ENTIRE subscriber on a full buffer (slow browser must never stall).
//
// Channel discipline: sends and the subscriber close BOTH hold s.mu —
// publishing never races a closing send (the go runtime panics on a send
// to a closed channel; the mutex orders them).
func (s *Stream) publish(subs []*subscriber, ev Event) {
	for _, sub := range subs {
		if sub.send(ev) {
			continue
		}
		// Gone (closed by send above, or by its cancel goroutine): stop tracking
		// it. The close itself happened under the subscriber's own lock, so this
		// is bookkeeping, not the race-safe boundary.
		s.subsMu.Lock()
		delete(s.subs, sub)
		s.subsMu.Unlock()
	}
}

func hex6(n uint64) string {
	return fmt.Sprintf("%06x", n)
}
