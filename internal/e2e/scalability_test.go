package e2e

// Scalability. Two claims, chosen so they do not depend on which
// machine runs them:
//
//  1. Authentication no longer costs a KDF per request. An ABSOLUTE latency
//     ceiling re-measures the host (the existing budget test already documents
//     how far a shared runner drifts); the machine-independent claim is the GAP
//     between the auth-on path and the auth-off path. With Argon2id (19 MiB,
//     t=2) that gap WAS the KDF, so this is the RED-GREEN for replacing it with
//     a keyed MAC.
//
//  2. Concurrent requests stay CORRECT with a connection pool. Every request
//     answers, every one persists its row, none fails. That is what grounds
//     raising the store's connection limit from one; a p99 measured here would
//     be this box's noise, not the proxy's behaviour, so the assertions are
//     correctness and the timings are logged.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/auth"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/testutil/mockprovider"
)

const chatOKBody = `{"id":"c1","object":"chat.completion","model":"gpt-5",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

const chatPlainBody = `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`

// scriptOK enqueues n identical successful chat completions.
func scriptOK(t *testing.T, e *env, n int) {
	t.Helper()
	s := mockprovider.NewScript().JSONResponse(http.StatusOK, chatOKBody, nil)
	for i := 0; i < n; i++ {
		e.mock.Enqueue(s)
	}
}

// p99Against posts n sequential requests to url and returns the p99 latency in
// milliseconds.
func p99Against(t *testing.T, client *http.Client, url, key string, n int) float64 {
	t.Helper()
	lat := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		req, err := http.NewRequest(http.MethodPost, url+"/v1/chat/completions",
			bytes.NewReader([]byte(chatPlainBody)))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d on request %d", resp.StatusCode, i)
		}
		lat = append(lat, float64(time.Since(start).Microseconds())/1000.0)
	}
	return percentile(lat, 99)
}

// The keyed-MAC change, measured as a GAP rather than an absolute ceiling.
func TestAuthCostIsNotAKdfPerRequest(t *testing.T) {
	if testing.Short() {
		t.Skip("measures wall-clock latency (skipped in -short)")
	}
	if raceEnabled {
		// The gap is measured in milliseconds and the bound is absolute; race
		// instrumentation inflates both paths ~40x, which makes a 3 ms bound a
		// statement about the detector rather than about the code. Same reasoning
		// as TestPerfBudgets, which skips for the same reason.
		t.Skip("race detector instrumentation inflates wall-clock latency; the assertion is enforced uninstrumented")
	}
	e := newEnv(t)
	ctx := context.Background()
	// Wire the client-key hasher the way main does, BEFORE the key is created:
	// without it the store writes a legacy Argon2id row and every request pays
	// the KDF — which is exactly the state this test exists to rule out (and
	// what it measured: a 71 ms gap).
	e.store.SetKeyHasher(storage.NewKeyHasher([]byte("master-key-material-for-tests")))
	_, key, err := auth.CreateClientKey(ctx, e.store, "scalability")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	// The auth-off mirror of the same handler: identical proxy path, no key
	// check. The difference between the two IS the auth cost.
	openSrv := httptest.NewServer(auth.NewMiddleware(e.store, auth.WithAuthDisabled()).Wrap(e.h))
	t.Cleanup(openSrv.Close)

	client := &http.Client{Timeout: 30 * time.Second}
	const samples = 120

	// Warm both paths (translation init, SQLite page cache, connection setup).
	scriptOK(t, e, 2*samples+40)
	for i := 0; i < 20; i++ {
		p99Against(t, client, openSrv.URL, "", 1)
		p99Against(t, client, e.srv.URL, key, 1)
	}

	openP99 := p99Against(t, client, openSrv.URL, "", samples)
	authP99 := p99Against(t, client, e.srv.URL, key, samples)
	gap := authP99 - openP99
	t.Logf("p99 auth-off = %.2f ms, auth-on = %.2f ms, gap = %.2f ms", openP99, authP99, gap)

	// The auth-on path still does two indexed SQLite reads and one write per
	// request, so the gap is not zero — but it must be a small fraction of a
	// request, not the 10-40 ms a 19 MiB KDF costs.
	const maxGapMS = 3.0
	if gap > maxGapMS {
		t.Errorf("auth adds %.2f ms per request (auth-off %.2f ms, auth-on %.2f ms) — want <= %.0f ms; a KDF-shaped gap means the keyed-MAC path is not in use",
			gap, openP99, authP99, maxGapMS)
	}
}

// Concurrent correctness under the store's connection pool. This is the gate
// that makes raising MaxOpenConns safe to reason about: if the pool or the busy
// timeout were wrong, requests would fail or their rows would go missing.
func TestConcurrentRequestsStayCorrect(t *testing.T) {
	e := newEnv(t)
	const n = 40

	scriptOK(t, e, n)

	client := &http.Client{Timeout: 30 * time.Second}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/v1/chat/completions",
				bytes.NewReader([]byte(chatPlainBody)))
			if err != nil {
				errs[i] = err
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer not-a-key") // auth-off-by-env is NOT used here: exercise the real gate
			resp, err := client.Do(req)
			if err != nil {
				errs[i] = err
				return
			}
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.Copy(io.Discard, resp.Body)
			if resp.StatusCode != http.StatusUnauthorized {
				errs[i] = fmt.Errorf("status %d, want 401 for an unknown key", resp.StatusCode)
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent request %d: %v", i, err)
		}
	}
	t.Logf("%d concurrent unauthenticated requests answered without a store error", n)

	// Now the authorized burst: every request must be served AND logged.
	e2 := newEnv(t)
	e2.store.SetKeyHasher(storage.NewKeyHasher([]byte("master-key-material-for-tests")))
	_, key, err := auth.CreateClientKey(context.Background(), e2.store, "burst")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	scriptOK(t, e2, n)

	var wg2 sync.WaitGroup
	errs2 := make([]error, n)
	for i := 0; i < n; i++ {
		wg2.Add(1)
		go func(i int) {
			defer wg2.Done()
			req, err := http.NewRequest(http.MethodPost, e2.srv.URL+"/v1/chat/completions",
				bytes.NewReader([]byte(chatPlainBody)))
			if err != nil {
				errs2[i] = err
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+key)
			resp, err := client.Do(req)
			if err != nil {
				errs2[i] = err
				return
			}
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.Copy(io.Discard, resp.Body)
			if resp.StatusCode != http.StatusOK {
				errs2[i] = fmt.Errorf("status %d", resp.StatusCode)
			}
		}(i)
	}
	wg2.Wait()
	for i, err := range errs2 {
		if err != nil {
			t.Fatalf("authorized request %d: %v", i, err)
		}
	}

	logs := e2.requestLogs(t)
	if len(logs) != n {
		t.Fatalf("request rows = %d, want %d — every concurrent request must persist exactly one row", len(logs), n)
	}
	for _, l := range logs {
		if l.Status != http.StatusOK {
			t.Errorf("row status = %d, want 200 (a store-level failure would surface here)", l.Status)
		}
	}
	t.Logf("%d concurrent authorized requests all served and logged", n)
}
