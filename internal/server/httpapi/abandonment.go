package httpapi

// How a request that nobody finished reading is recorded.

import (
	"context"
	"errors"
	"net/http"
)

// Request-log markers for a request that ended without an answer.
//
// Nothing goes on the wire for either of them — the client is gone, or the
// answer is already delivered — so these exist only to make the log row say what
// happened instead of claiming a 200. A 200 for an abandoned stream is the one
// thing the log must never say: it is indistinguishable from a request that was
// served.
const (
	// statusClientClosedRequest: the client stopped reading and closed the
	// connection before the answer was complete.
	statusClientClosedRequest = 499

	// statusAbandonedBudget: the request ended because a budget of OURS ran
	// out — an emulation loop cap, a deadline we set. Distinct from 499 on
	// purpose: writing our own deadline as a client departure sends the
	// operator looking at the wrong end of the connection.
	statusAbandonedBudget = http.StatusGatewayTimeout
)

// abandonmentStatus reports the log status for a request whose context has
// ended, and whether it has. Read after the response has been served, which is
// where the distinction becomes meaningful: a live request has a live context,
// and a nil error means the client is still there and the answer status stands.
func abandonmentStatus(ctx context.Context) (status int, abandoned bool) {
	err := ctx.Err()
	switch {
	case err == nil:
		return 0, false
	case errors.Is(err, context.DeadlineExceeded):
		return statusAbandonedBudget, true
	default:
		return statusClientClosedRequest, true
	}
}

// mapAbandonment maps a pipeline error that ended in a cancellation onto the
// same two markers, so the paths that fail before a stream starts agree with
// the ones that fail during it.
func mapAbandonment(err error) (int, bool) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return statusAbandonedBudget, true
	case errors.Is(err, context.Canceled):
		return statusClientClosedRequest, true
	default:
		return 0, false
	}
}
