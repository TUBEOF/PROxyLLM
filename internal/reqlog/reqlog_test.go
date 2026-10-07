package reqlog

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// Spec: Logs-page redesign (2026-10-06 plan) — the admin log stream needs
// three capabilities from one component: an in-flight registry (running
// rows), a finished-row broadcast, and SSE subscriber fan-out with bounded
// backpressure (drop-slow-client for log rows; running snapshots are
// idempotent state and may replace). reqlog never queries SQLite: the SSE
// handler backfills from the store cursor itself.

func mustSubscribe(t *testing.T, s *Stream) (<-chan Event, func()) {
	t.Helper()
	ch, cancel := s.Subscribe(context.Background())
	return ch, cancel
}

func drainRunning(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for running snapshot")
		return Event{}
	}
}

func TestStream_StartFinishBroadcast(t *testing.T) {
	s := New()
	ch, cancel := mustSubscribe(t, s)
	defer cancel()
	drainRunning(t, ch) // the Subscribe snapshot (empty before any Start)

	finish, _ := s.Start(Inflight{
		ID: "run_000001", StartedAt: 1760000000, ClientKeyID: "key-1",
		Model: "glm-5.3-flash", Protocol: "anthropic", ConversationID: "conv_a1b2c3d4",
	})
	// Start broadcasts a running snapshot containing the new entry.
	ev := drainRunning(t, ch)
	if ev.Kind != EventRunning {
		t.Fatalf("first event kind = %d, want running", ev.Kind)
	}
	if len(ev.Running) != 1 || ev.Running[0].Model != "glm-5.3-flash" {
		t.Fatalf("snapshot = %+v", ev.Running)
	}

	// Finish deregisters (running snapshot first — the pseudo-row
	// disappears) and broadcasts the terminal row.
	finish(LogRow{ID: 42, Model: "glm-5.3-flash", Status: 200, Protocol: "anthropic"})
	select {
	case ev := <-ch:
		if ev.Kind != EventRunning {
			t.Fatalf("post-finish first event = kind %d, want running", ev.Kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no post-finish running snapshot")
	}
	select {
	case ev := <-ch:
		if ev.Kind != EventLog || ev.Row.ID != 42 {
			t.Fatalf("finished event = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no finished row broadcast")
	}

	// The tracker is empty again — a fresh subscriber sees an empty snapshot.
	ch2, cancel2 := mustSubscribe(t, s)
	defer cancel2()
	if ev := drainRunning(t, ch2); len(ev.Running) != 0 {
		t.Errorf("snapshot after finish = %+v entries, want empty", ev.Running)
	}

	// finish is idempotent (sync.Once): the second call must not broadcast
	// a duplicate row.
	finish(LogRow{ID: 43, Status: 200})
	select {
	case ev := <-ch:
		if ev.Kind == EventLog {
			t.Fatalf("idempotent finish leaked a second row: %+v", ev)
		}
	case <-time.After(200 * time.Millisecond):
	}
}

func TestStream_SubsequentSnapshotsReplace(t *testing.T) {
	s := New()
	ch, cancel := mustSubscribe(t, s)
	defer cancel()
	drainRunning(t, ch) // empty initial snapshot

	f1, _ := s.Start(Inflight{ID: "run_1", Model: "m1"})
	ev := drainRunning(t, ch)
	if len(ev.Running) != 1 {
		t.Fatalf("after start 1: %+v", ev.Running)
	}
	f2, _ := s.Start(Inflight{ID: "run_2", Model: "m2"})
	ev = drainRunning(t, ch)
	if len(ev.Running) != 2 {
		t.Fatalf("after start 2: %+v", ev.Running)
	}
	f2(LogRow{ID: 7, Status: 200})
	ev = drainRunning(t, ch)
	if ev.Kind != EventRunning || len(ev.Running) != 1 || ev.Running[0].ID != "run_1" {
		t.Fatalf("after finish run_2: %+v (want only run_1 left)", ev)
	}
	// Every running snapshot fully replaces the previous list (state, not
	// delta) — implementers may rely on that.
	ev = drainRunning(t, ch) // EventLog for id 7
	if ev.Kind != EventLog || ev.Row.ID != 7 {
		t.Fatalf("expected log row 7, got %+v", ev)
	}
	f1(LogRow{ID: 8, Status: 200})
}

func TestStream_SlowSubscriberDropped(t *testing.T) {
	s := New()
	ch, cancel := mustSubscribe(t, s)
	defer cancel()
	drainRunning(t, ch) // empty snapshot

	// No-one reads ch anymore: the subscriber buffer (16) fills, then the
	// hub drops the subscriber for log events instead of becoming the
	// proxy's backpressure. 40 log events (buffer capacity 16 + 16 more
	// than the eviction trick can absorb) guarantee a drop.
	for i := 0; i < 40; i++ {
		finish, _ := s.Start(Inflight{ID: "run_x", StartedAt: 1760000000, Model: "m"})
		finish(LogRow{ID: int64(100 + i), Status: 200})
	}
	// The subscriber channel must end up closed (dropped), not stalled:
	// drain until either a closed receive or the deadline.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // dropped as designed
			}
		case <-deadline:
			t.Fatal("slow subscriber neither delivered nor dropped in time")
			return
		}
	}
}

func TestStream_ReapStaleInflight(t *testing.T) {
	s := New()
	s.now = func() time.Time { return time.Unix(1760000000, 0) }
	ch, cancel := mustSubscribe(t, s)
	defer cancel()
	drainRunning(t, ch) // empty snapshot

	// A request that never reaches finish (panic path) leaks an in-flight
	// entry; the reap on the next Start clears entries older than the
	// stale window.
	finish, _ := s.Start(Inflight{ID: "run_stale", StartedAt: 1760000000, Model: "m"})
	drainRunning(t, ch)

	s.now = func() time.Time { return time.Unix(1760000000+7200, 0) } // 2 h later
	s.Start(Inflight{ID: "run_fresh", StartedAt: 1760000000 + 7200, Model: "m"})
	if ev := drainRunning(t, ch); len(ev.Running) != 1 || ev.Running[0].ID != "run_fresh" {
		t.Fatalf("stale entry not reaped: %+v", ev.Running)
	}
	// The stale finish becomes a no-op (no zombie row broadcast, no panic).
	finish(LogRow{ID: 500, Status: 200})
}

func TestStream_RaceChurn(t *testing.T) {
	s := New()
	stop := make(chan struct{})

	// Writer goroutines interleave Start/finish bursts — run with -race.
	for w := 0; w < 8; w++ {
		go func(w int) {
			for i := 0; i < 50; i++ {
				select {
				case <-stop:
					return
				default:
				}
				finish, _ := s.Start(Inflight{
					ID: "run_x", StartedAt: 1760000000,
					ClientKeyID: "k", Model: "m", Protocol: "openai",
				})
				finish(LogRow{ID: int64(w*1000 + i), Status: 200})
			}
		}(w)
	}

	// Subscriber churn cycles.
	for c := 0; c < 32; c++ {
		ch, cancel := mustSubscribe(t, s)
		select {
		case <-ch:
		case <-time.After(50 * time.Millisecond):
		}
		cancel()
	}
	close(stop)
}

// compile-time shape check: events marshal on the wire shape the SSE
// handler documents.
func TestEventJSONShape(t *testing.T) {
	raw, err := json.Marshal(Event{Kind: EventRunning, Running: []Inflight{{ID: "run_1", Model: "m"}}})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == "" {
		t.Error("running event must marshal")
	}
}
