package httpapi

// The wire status for an exhausted chain must follow docs/api/errors.md:
// a chain whose every attempt died before an HTTP response is a gateway
// failure (502), and a timeout among those attempts is 504. Before these
// tests, an untyped transport error fell through to 500.

import (
	"context"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"

	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/translate/ir"
)

type mapProvider struct {
	id  string
	err error
}

func (p mapProvider) ID() string                               { return p.id }
func (p mapProvider) Capabilities() provider.Capabilities      { return provider.Capabilities{} }
func (p mapProvider) RefreshCredentials(context.Context) error { return nil }

func (p mapProvider) Complete(context.Context, ir.Request) (ir.Response, error) {
	return ir.Response{}, p.err
}

func (p mapProvider) Stream(context.Context, ir.Request) (<-chan ir.StreamEvent, func(), error) {
	return nil, func() {}, p.err
}

type mapTimeout struct{}

func (mapTimeout) Error() string   { return "http2: timeout awaiting response headers" }
func (mapTimeout) Timeout() bool   { return true }
func (mapTimeout) Temporary() bool { return true }

func mapRefused() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
}

func mapUpErr(status int, code string) error {
	return &provider.UpstreamError{HTTPStatus: status, Code: code, Message: "test"}
}

// chainError runs one chain through a real executor and returns its terminal
// error, so the mapping sees the production error value (not a hand-built
// stand-in for the unexported exhaustedError).
func chainError(t *testing.T, entries []routing.ChainEntry, provs ...provider.Provider) error {
	t.Helper()
	reg := provider.NewRegistry()
	for _, p := range provs {
		reg.Register(p)
	}
	e := routing.NewExecutor(routing.ExecutorConfig{
		Registry:     reg,
		RetrySleeper: func(time.Duration) {},
	})
	_, _, err := e.Execute(context.Background(), entries, ir.Request{Model: "m"})
	if err == nil {
		t.Fatal("Execute succeeded, want an exhausted chain")
	}
	return err
}

func TestMapRouteError_TransportExhaustion(t *testing.T) {
	t.Run("timeout is 504", func(t *testing.T) {
		err := chainError(t, []routing.ChainEntry{{ProviderID: "p1", Model: "m"}},
			mapProvider{id: "p1", err: mapTimeout{}})
		status, _, _ := mapRouteError(err)
		if status != http.StatusGatewayTimeout {
			t.Fatalf("status = %d, want 504 (%v)", status, err)
		}
	})

	t.Run("connection reset is 502", func(t *testing.T) {
		err := chainError(t, []routing.ChainEntry{{ProviderID: "p1", Model: "m"}},
			mapProvider{id: "p1", err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}})
		status, _, _ := mapRouteError(err)
		if status != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 (%v)", status, err)
		}
	})

	t.Run("dial failure is 502", func(t *testing.T) {
		err := chainError(t, []routing.ChainEntry{{ProviderID: "p1", Model: "m"}},
			mapProvider{id: "p1", err: mapRefused()})
		status, _, _ := mapRouteError(err)
		if status != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 (%v)", status, err)
		}
	})
}

func TestMapRouteError_UniformRateLimitStays429(t *testing.T) {
	err := chainError(t, []routing.ChainEntry{{ProviderID: "p1", Model: "m"}},
		mapProvider{id: "p1", err: mapUpErr(http.StatusTooManyRequests, "rate_limit_exceeded")})
	status, _, _ := mapRouteError(err)
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (%v)", status, err)
	}
}

func TestMapRouteError_MixedChainStays502(t *testing.T) {
	err := chainError(t, []routing.ChainEntry{{ProviderID: "p1", Model: "m"}, {ProviderID: "p2", Model: "m"}},
		mapProvider{id: "p1", err: mapUpErr(http.StatusTooManyRequests, "rate_limit_exceeded")},
		mapProvider{id: "p2", err: mapUpErr(http.StatusInternalServerError, "server_error")})
	status, _, _ := mapRouteError(err)
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (%v)", status, err)
	}
}

func TestMapRouteError_ZeroAttemptChainStays500(t *testing.T) {
	err := chainError(t, []routing.ChainEntry{{ProviderID: "missing", Model: "m"}})
	status, _, _ := mapRouteError(err)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for a walk that made no attempt (%v)", status, err)
	}
}

func TestMapRouteError_UniformCapabilityGapUnchanged(t *testing.T) {
	err := chainError(t, []routing.ChainEntry{{ProviderID: "p1", Model: "m"}},
		mapProvider{id: "p1", err: &provider.UpstreamError{
			HTTPStatus: http.StatusBadRequest,
			Code:       "unsupported_content",
			Message:    "images are not supported",
		}})
	status, _, _ := mapRouteError(err)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%v)", status, err)
	}
}
