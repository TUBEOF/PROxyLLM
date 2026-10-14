package httpapi

// A request the client walked away from is still a request: it gets a log row,
// and that row says what ended it. Two markers, because "the client stopped
// reading" and "a budget of ours ran out" are different facts — and neither is
// a 200: an abandoned stream is not a success.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/testutil/mockprovider"
)

func TestAbandonmentStatus_DistinguishesClientFromBudget(t *testing.T) {
	// A live request: the answer's own status stands.
	if status, abandoned := abandonmentStatus(context.Background()); abandoned || status != 0 {
		t.Errorf("live context → %d/%v, want no marker", status, abandoned)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	status, abandoned := abandonmentStatus(ctx)
	if !abandoned || status != statusClientClosedRequest {
		t.Errorf("cancelled context → %d/%v, want %d", status, abandoned, statusClientClosedRequest)
	}

	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	status, abandoned = abandonmentStatus(expired)
	if !abandoned || status != statusAbandonedBudget {
		t.Errorf("expired deadline → %d/%v, want %d (our own deadline is not a client departure)",
			status, abandoned, statusAbandonedBudget)
	}
}

// The same distinction on the paths that fail before a stream starts, where the
// status also reaches the wire.
func TestMapRouteError_MarksCancellationSeparatelyFromAnExpiredDeadline(t *testing.T) {
	if status, _, _ := mapRouteError(context.Canceled); status != statusClientClosedRequest {
		t.Errorf("cancelled → %d, want %d", status, statusClientClosedRequest)
	}
	if status, _, _ := mapRouteError(context.DeadlineExceeded); status != statusAbandonedBudget {
		t.Errorf("deadline → %d, want %d", status, statusAbandonedBudget)
	}
}

// End to end: a client that reads the first event and hangs up leaves a row
// that is not a 200, and leaves the provider's health alone — nobody learned
// anything about that provider from this request.
func TestStream_AbandonedRequestIsLoggedWithItsMarker(t *testing.T) {
	e := newEnv(t)
	// One event, then silence: long enough for the client to walk away while
	// the upstream is still holding the stream open.
	e.mock.Enqueue(mockprovider.NewScript().
		Chunk(`{"id":"chunk-1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"hello"}}]}`).
		Delay(1500 * time.Millisecond).
		Chunk(`{"id":"chunk-2","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":"stop"}]}`))

	body := `{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	buf := make([]byte, 64)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("first event read: %v", err)
	}
	// The client has what it wanted from the first frame and leaves.
	cancel()
	_ = resp.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		logs := e.requestLogs(t)
		if len(logs) > 0 {
			row := logs[0]
			if row.Status == http.StatusOK {
				t.Fatalf("abandoned stream logged as 200; the row cannot be told apart from a served answer")
			}
			if row.Status != statusClientClosedRequest {
				t.Errorf("status = %d, want %d", row.Status, statusClientClosedRequest)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no log row was written for the abandoned request")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Nothing was learned about the provider, so its health is untouched — an
	// abandoned request must not close a breaker either.
	if reason := e.health.Reason("mock-main"); reason != "" {
		t.Errorf("provider health = %q after an abandoned request, want untouched", reason)
	}
}
