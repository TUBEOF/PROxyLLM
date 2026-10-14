package routing

// Per-account circuit and account-attributed trace events.
//
// Spec: a web-tool account that is down or misconfigured used to be retried
// FIRST on every call, paying its failure (up to the call budget) before the
// walk moved on — so one bad account slowed every request, and the log could not
// say which account had been asked.
//
// In-package so the cooldown clock is injectable: a test against a 30 s wall
// wait would be a test nobody runs.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// acctEmulator is one identified account: it answers, answers empty, or fails.
type acctEmulator struct {
	calls int
	err   error
	empty bool
}

func (a *acctEmulator) Search(_ context.Context, _ string, _ ir.ServerToolConfig) ([]ir.WebSearchResultItem, error) {
	a.calls++
	if a.err != nil {
		return nil, a.err
	}
	if a.empty {
		return nil, nil
	}
	return []ir.WebSearchResultItem{{URL: "https://go.dev/", Title: "Go"}}, nil
}

func (a *acctEmulator) Fetch(_ context.Context, _ string, _ ir.ServerToolConfig) (*ir.WebFetchContent, error) {
	a.calls++
	if a.err != nil {
		return nil, a.err
	}
	if a.empty {
		return &ir.WebFetchContent{Data: ""}, nil
	}
	return &ir.WebFetchContent{Data: "page"}, nil
}

func newPoolWithClock(search, fetch []EmulatorEntry, now *time.Time) *EmulatorPool {
	p := NewEmulatorPoolEntries(search, fetch)
	p.clock = func() time.Time { return *now }
	return p
}

func TestEmulatorPool_CoolsAFailingAccountAndSkipsIt(t *testing.T) {
	now := time.Unix(1_760_000_000, 0)
	bad := &acctEmulator{err: errors.New("account down")}
	good := &acctEmulator{}
	pool := newPoolWithClock(
		[]EmulatorEntry{{ID: "prv-bad", Emulator: bad}, {ID: "prv-good", Emulator: good}},
		nil, &now)

	// Three walks: the bad account fails each time, the good one answers.
	for i := 0; i < emuFailureThreshold; i++ {
		if _, err := pool.Search(context.Background(), "q", ir.ServerToolConfig{}); err != nil {
			t.Fatalf("walk %d: %v", i, err)
		}
	}
	if bad.calls != emuFailureThreshold {
		t.Fatalf("bad account calls = %d, want %d", bad.calls, emuFailureThreshold)
	}

	// Fourth walk: the bad account is cooling, so it is not asked at all.
	if _, err := pool.Search(context.Background(), "q", ir.ServerToolConfig{}); err != nil {
		t.Fatalf("walk after cooling: %v", err)
	}
	if bad.calls != emuFailureThreshold {
		t.Errorf("bad account calls = %d, want %d — a cooling account must not be tried", bad.calls, emuFailureThreshold)
	}

	// After the window it is tried again (a transient outage recovers).
	now = now.Add(emuCooldownBase + time.Second)
	if _, err := pool.Search(context.Background(), "q", ir.ServerToolConfig{}); err != nil {
		t.Fatalf("walk after cooldown: %v", err)
	}
	if bad.calls != emuFailureThreshold+1 {
		t.Errorf("bad account calls = %d, want one more after the cooldown expired", bad.calls)
	}
}

func TestEmulatorPool_AllCoolingWalksAnyway(t *testing.T) {
	now := time.Unix(1_760_000_000, 0)
	a := &acctEmulator{err: errors.New("a down")}
	b := &acctEmulator{err: errors.New("b down")}
	pool := newPoolWithClock(
		[]EmulatorEntry{{ID: "prv-a", Emulator: a}, {ID: "prv-b", Emulator: b}}, nil, &now)

	// Drive both accounts into cooldown.
	for i := 0; i < emuFailureThreshold; i++ {
		if _, err := pool.Search(context.Background(), "q", ir.ServerToolConfig{}); err == nil {
			t.Fatalf("walk %d: expected the last account's error", i)
		}
	}
	before := a.calls + b.calls

	// Every account cooling: the walk must still be attempted (a probe), or a
	// family that had a bad minute could never recover.
	if _, err := pool.Search(context.Background(), "q", ir.ServerToolConfig{}); err == nil {
		t.Error("expected the last account's error from the probe walk")
	}
	if a.calls+b.calls == before {
		t.Error("no account was probed — a fully cooling family must still be walked")
	}
}

func TestEmulatorPool_SuccessClearsTheCircuit(t *testing.T) {
	now := time.Unix(1_760_000_000, 0)
	flaky := &acctEmulator{err: errors.New("transient")}
	pool := newPoolWithClock([]EmulatorEntry{{ID: "prv-flaky", Emulator: flaky}}, nil, &now)

	// Two failures: below the threshold, so no cooldown yet.
	for i := 0; i < emuFailureThreshold-1; i++ {
		_, _ = pool.Search(context.Background(), "q", ir.ServerToolConfig{})
	}
	// It recovers.
	flaky.err = nil
	if _, err := pool.Search(context.Background(), "q", ir.ServerToolConfig{}); err != nil {
		t.Fatalf("recovery walk: %v", err)
	}

	// The counter is gone: two more failures must not cool it down.
	flaky.err = errors.New("transient again")
	for i := 0; i < emuFailureThreshold-1; i++ {
		_, _ = pool.Search(context.Background(), "q", ir.ServerToolConfig{})
	}
	callsBefore := flaky.calls
	if _, err := pool.Search(context.Background(), "q", ir.ServerToolConfig{}); err == nil {
		t.Fatal("expected the account's error")
	}
	if flaky.calls != callsBefore+1 {
		t.Error("the account was skipped — a success must reset its failure count")
	}
}

func TestEmulatorPool_SearchTracedNamesEachAccount(t *testing.T) {
	empty := &acctEmulator{empty: true}
	broken := &acctEmulator{err: errors.New("broken down")}
	good := &acctEmulator{}
	pool := NewEmulatorPoolEntries([]EmulatorEntry{
		{ID: "prv-empty", Emulator: empty},
		{ID: "prv-broken", Emulator: broken},
		{ID: "prv-good", Emulator: good},
	}, nil)

	var trace Trace
	items, by, err := pool.SearchTraced(context.Background(), "golang", ir.ServerToolConfig{}, &trace)
	if err != nil {
		t.Fatalf("SearchTraced: %v", err)
	}
	if by != "prv-good" {
		t.Errorf("answering account = %q, want the one that answered", by)
	}
	if len(items) != 1 {
		t.Errorf("items = %+v, want the answering account's result", items)
	}

	// Every account's outcome is in the trace, in walk order: this is what makes
	// "the web tool found nothing" answerable from the log.
	var got []string
	for _, ev := range trace.Events() {
		if ev.Type != EventEmulatedToolAttempt {
			continue
		}
		got = append(got, ev.ProviderID+"="+ev.Reason)
		if ev.To != "web_search" {
			t.Errorf("attempt event tool = %q, want web_search", ev.To)
		}
	}
	want := []string{"prv-empty=empty", "prv-broken=failed", "prv-good=answered"}
	if len(got) != len(want) {
		t.Fatalf("attempt events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("attempt event[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// A pool built without identity (the legacy constructor) reports no account —
// there is nothing to name — and stays otherwise unchanged.
func TestEmulatorPool_AnonymousPoolRecordsNoAttempts(t *testing.T) {
	pool := NewEmulatorPool([]Emulator{&acctEmulator{}}, nil)
	var trace Trace
	if _, by, err := pool.SearchTraced(context.Background(), "q", ir.ServerToolConfig{}, &trace); err != nil {
		t.Fatalf("SearchTraced: %v", err)
	} else if by != "" {
		t.Errorf("answering account = %q, want none (no identity)", by)
	}
	for _, ev := range trace.Events() {
		if ev.Type == EventEmulatedToolAttempt {
			t.Errorf("anonymous pool recorded an attempt event: %+v", ev)
		}
	}
}
