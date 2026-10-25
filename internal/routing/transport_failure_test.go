package routing_test

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"

	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// headerTimeoutErr is the shape Go's HTTP/2 transport returns when an
// upstream accepts the connection and then produces no response headers:
// a net.Error whose Timeout() is true.
type headerTimeoutErr struct{}

func (headerTimeoutErr) Error() string   { return "http2: timeout awaiting response headers" }
func (headerTimeoutErr) Timeout() bool   { return true }
func (headerTimeoutErr) Temporary() bool { return true }

// refusedErr is a dial failure: the request never reached the provider.
func refusedErr() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
}

// TestChainTransportFailure covers the status-mapping predicate derived from
// docs/api/errors.md: "transport failure to every attempted provider" is a
// gateway problem (502), and a timeout among them is 504. Only an exhausted
// chain whose attempts ALL failed in the transport/network classes qualifies.
// A uniform 429, a capability gap, or a mixed chain must not qualify.
func TestChainTransportFailure(t *testing.T) {
	t.Run("uniform transport timeout", func(t *testing.T) {
		p := &scriptedProvider{id: "p1", completeErrs: []error{headerTimeoutErr{}}}
		e := newExecutor(t, p)
		_, _, err := e.Execute(context.Background(), chain("p1"), ir.Request{Model: "m"})
		timedOut, ok := routing.ChainTransportFailure(err)
		if !ok || !timedOut {
			t.Fatalf("ChainTransportFailure = (%v, %v), want (true, true): %v", timedOut, ok, err)
		}
	})

	t.Run("uniform transport non-timeout", func(t *testing.T) {
		p := &scriptedProvider{id: "p1", completeErrs: []error{errors.New("connection reset by peer")}}
		e := newExecutor(t, p)
		_, _, err := e.Execute(context.Background(), chain("p1"), ir.Request{Model: "m"})
		timedOut, ok := routing.ChainTransportFailure(err)
		if !ok || timedOut {
			t.Fatalf("ChainTransportFailure = (%v, %v), want (false, true): %v", timedOut, ok, err)
		}
	})

	t.Run("uniform network dial failure", func(t *testing.T) {
		p := &scriptedProvider{id: "p1", completeErrs: []error{refusedErr()}}
		e := newExecutor(t, p)
		_, _, err := e.Execute(context.Background(), chain("p1"), ir.Request{Model: "m"})
		timedOut, ok := routing.ChainTransportFailure(err)
		if !ok || timedOut {
			t.Fatalf("ChainTransportFailure = (%v, %v), want (false, true): %v", timedOut, ok, err)
		}
	})

	t.Run("mixed chain does not qualify", func(t *testing.T) {
		// Two providers: the first times out, the second answers 500. The
		// classes differ, so the mixed-chain mapping owns the status.
		first := &scriptedProvider{id: "p1", completeErrs: []error{headerTimeoutErr{}}}
		second := &scriptedProvider{id: "p2", completeErrs: []error{upErr(500, "server_error")}}
		e := newExecutor(t, first, second)
		_, _, err := e.Execute(context.Background(), chain("p1", "p2"), ir.Request{Model: "m"})
		if _, ok := routing.ChainTransportFailure(err); ok {
			t.Fatalf("mixed chain qualified as transport-only: %v", err)
		}
	})

	t.Run("uniform rate limit does not qualify", func(t *testing.T) {
		p := &scriptedProvider{id: "p1", completeErrs: []error{
			upErr(429, "rate_limit_exceeded"),
			upErr(429, "rate_limit_exceeded"),
			upErr(429, "rate_limit_exceeded"),
		}}
		e := newExecutor(t, p)
		_, _, err := e.Execute(context.Background(), chain("p1"), ir.Request{Model: "m"})
		if _, ok := routing.ChainTransportFailure(err); ok {
			t.Fatalf("uniform 429 qualified as transport: %v", err)
		}
	})

	t.Run("not an exhausted chain", func(t *testing.T) {
		if _, ok := routing.ChainTransportFailure(errors.New("boom")); ok {
			t.Fatal("a plain error qualified as an exhausted chain")
		}
	})
}
