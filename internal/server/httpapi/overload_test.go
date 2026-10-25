package httpapi

// The 529/503 overload answer must name the cooldown it is: a circuit-breaker
// trip and an upstream quota/rate-limit window previously shared one message
// that always claimed "circuit breaker". The typed facts also drive
// Retry-After so a client can wait out the earliest recovery.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/routing"
)

func TestOverloadMessage_NamesTheCooldown(t *testing.T) {
	t.Run("breaker", func(t *testing.T) {
		err := &routing.ChainOverloadedError{Reason: "breaker", Until: time.Now().Add(2500 * time.Millisecond)}
		if !errors.Is(err, routing.ErrChainOverloaded) {
			t.Fatal("typed overload error does not match the sentinel")
		}
		msg, retryAfter := overloadMessage(err)
		if !strings.Contains(msg, "circuit breaker") {
			t.Errorf("message = %q, want circuit breaker", msg)
		}
		if retryAfter != 3 {
			t.Errorf("retryAfter = %d, want 3 (ceil of 2.5s)", retryAfter)
		}
	})

	t.Run("quota", func(t *testing.T) {
		err := &routing.ChainOverloadedError{Reason: "quota_exhausted", Until: time.Now().Add(10 * time.Second)}
		msg, retryAfter := overloadMessage(err)
		if !strings.Contains(msg, "quota/rate-limit") {
			t.Errorf("message = %q, want quota/rate-limit", msg)
		}
		if retryAfter < 9 || retryAfter > 10 {
			t.Errorf("retryAfter = %d, want 9..10", retryAfter)
		}
	})

	t.Run("sentinel only keeps the historical wording", func(t *testing.T) {
		msg, retryAfter := overloadMessage(routing.ErrChainOverloaded)
		if !strings.Contains(msg, "circuit breaker") || !strings.Contains(msg, "Retry shortly") {
			t.Errorf("message = %q, want the historical wording", msg)
		}
		if retryAfter != 0 {
			t.Errorf("retryAfter = %d, want 0 without a deadline", retryAfter)
		}
	})
}

func TestWriteRouteError_SetsRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()
	ew := errorWriter{
		write: func(w http.ResponseWriter, status int, errType, message string) {
			w.WriteHeader(status)
		},
		overloadStatus: 529,
		overloadType:   "overloaded_error",
	}
	err := &routing.ChainOverloadedError{Reason: "breaker", Until: time.Now().Add(4 * time.Second)}
	status, msg := (&api{}).writeRouteError(rec, ew, err)
	if status != 529 || rec.Code != 529 {
		t.Fatalf("status = %d / recorder %d, want 529", status, rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "4" {
		t.Errorf("Retry-After = %q, want 4", got)
	}
	if !strings.Contains(msg, "circuit breaker") {
		t.Errorf("message = %q, want circuit breaker", msg)
	}
}
