package codex

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Stepwise device flow — the form the admin API drives: StartDeviceLogin
// surfaces the user_code and verification URL to the operator (the blocking
// LoginFromDevice keeps them internal, but the operator must enter the code on
// the verification page — a UI-driven flow has to show it), and Poll performs
// exactly ONE issuer round trip per call, throttled to the issuer-provided
// interval, so the SPA polls at the issuer's pace rather than its own.
//
// LoginFromDevice (auth.go) delegates to these primitives; the wire behavior
// is identical (shared start/poll/exchange code paths).

// DeviceSession is one stepwise device login. Created by StartDeviceLogin;
// consumed via Poll until approved or failed. Safe for one poller goroutine; a
// concurrent Poll inside the interval window coalesces onto the in-flight (or
// just-completed) round trip instead of hitting the issuer a second time.
type DeviceSession struct {
	// DeviceAuthID + UserCode are the wire pair the poll body carries.
	DeviceAuthID string
	UserCode     string
	// VerificationURL is where the operator enters the code: {issuer}/codex/device.
	VerificationURL string
	// Interval is the parsed poll interval — a STRING on the wire, in
	// seconds. Callers pace their Poll calls to it.
	Interval time.Duration

	// deadline is the hard cap for the whole flow (CLI: 15 min).
	deadline time.Time

	auth *authClient

	mu       sync.Mutex
	lastPoll time.Time     // start of the last issuer poll (throttle window)
	flight   *deviceFlight // non-nil while a poll round trip is in flight
	done     bool          // approval exchanged & persisted — terminal
	err      error         // terminal failure (fatal poll/exchange error)
}

// deviceFlight is one in-flight poll round trip: waiters coalesce onto it
// instead of issuing a second issuer call inside the interval window.
type deviceFlight struct {
	done chan struct{}
	// approved/err are written once, before the close — waiters read them
	// after <-done, so no further synchronization is needed.
	approved bool
	err      error
}

// StartDeviceLogin requests the device code and returns the session the caller
// drives with Poll. A 404 on the usercode request is device-login disabled — an
// operator error, not a retry candidate.
func (a *authClient) StartDeviceLogin(ctx context.Context, timeout time.Duration) (*DeviceSession, error) {
	if timeout <= 0 {
		timeout = 15 * time.Minute // hard cap (CLI: 15 min)
	}
	var uc struct {
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
		Interval     string `json:"interval"`
	}
	err := a.postJSON(ctx, a.issuer+"/api/accounts/deviceauth/usercode",
		struct {
			ClientID string `json:"client_id"`
		}{ClientID: a.clientID}, &uc)
	if err != nil {
		var ee *endpointError
		if errors.As(err, &ee) && ee.status == http.StatusNotFound {
			// 404 on the usercode request = device login disabled.
			return nil, &DeviceLoginDisabledError{Detail: "issuer answered 404 on the device-code request"}
		}
		return nil, err
	}
	if uc.DeviceAuthID == "" || uc.UserCode == "" {
		return nil, errors.New("codex: device-code response missing device_auth_id/user_code")
	}
	interval := 5 * time.Second
	if v, perr := strconv.Atoi(strings.TrimSpace(uc.Interval)); perr == nil && v > 0 {
		interval = time.Duration(v) * time.Second
	}
	return &DeviceSession{
		DeviceAuthID:    uc.DeviceAuthID,
		UserCode:        uc.UserCode,
		VerificationURL: a.issuer + "/codex/device",
		Interval:        interval,
		deadline:        time.Now().Add(timeout),
		auth:            a,
	}, nil
}

// Poll performs one issuer poll, throttled to the issuer-provided interval:
// a call inside the window coalesces onto the in-flight/just-completed round
// trip and reports its outcome without a second issuer call (RFC 8628 §3.5
// interval pacing).
//
// Returns approved=true once the approval has been EXCHANGED and PERSISTED —
// tokens are on disk by the time the caller sees true. After approval the
// session is terminal: further Polls fail without issuer traffic (the
// authorization code is single-use; there is nothing left to poll). After a
// fatal error the recorded error replays the same way.
func (s *DeviceSession) Poll(ctx context.Context) (bool, LoginResult, error) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return false, LoginResult{}, errors.New("codex: device session already completed — the authorization code is single-use")
	}
	if s.err != nil {
		e := s.err
		s.mu.Unlock()
		return false, LoginResult{}, e
	}
	if s.flight != nil {
		f := s.flight
		s.mu.Unlock()
		<-f.done
		return f.approved, LoginResult{}, f.err
	}
	if time.Now().After(s.deadline) {
		s.mu.Unlock()
		return false, LoginResult{}, fmt.Errorf("codex: device login timed out")
	}
	if !s.lastPoll.IsZero() && time.Since(s.lastPoll) < s.Interval {
		s.mu.Unlock()
		return false, LoginResult{}, nil // inside the interval window — report pending
	}
	f := &deviceFlight{done: make(chan struct{})}
	s.flight = f
	s.lastPoll = time.Now()
	s.mu.Unlock()

	approval, err := s.pollIssuer(ctx)
	if err == nil && approval != nil {
		// Exchange + persist inside the flight so a coalesced waiter can never
		// observe approval without the tokens being on disk.
		res, err := s.auth.exchangeCode(ctx, approval.authorizationCode, s.auth.issuer+"/deviceauth/callback", approval.codeVerifier)
		if err != nil {
			s.fail(f, err)
			return false, LoginResult{}, err
		}
		s.auth.markRefreshed()
		s.mu.Lock()
		s.done = true
		if s.flight == f {
			s.flight = nil
		}
		s.mu.Unlock()
		f.approved = true
		close(f.done)
		return true, res, nil
	}
	if err != nil {
		s.fail(f, err)
		return false, LoginResult{}, err
	}
	// pending (403/404) — the flight reports it and the session stays live.
	s.mu.Lock()
	if s.flight == f {
		s.flight = nil
	}
	s.mu.Unlock()
	f.approved = false
	close(f.done)
	return false, LoginResult{}, nil
}

// deviceApproval is the success payload from the poll endpoint: the
// SERVER-provided PKCE pair consumed by the exchange.
type deviceApproval struct {
	authorizationCode string
	codeVerifier      string
}

// pollIssuer performs the issuer round trip: 403/404 = pending (nil approval,
// nil error), any other non-2xx = fatal, 2xx = approval.
func (s *DeviceSession) pollIssuer(ctx context.Context) (*deviceApproval, error) {
	var pollResp struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	err := s.auth.postJSON(ctx, s.auth.issuer+"/api/accounts/deviceauth/token",
		struct {
			DeviceAuthID string `json:"device_auth_id"`
			UserCode     string `json:"user_code"`
		}{DeviceAuthID: s.DeviceAuthID, UserCode: s.UserCode}, &pollResp)
	if err != nil {
		var ee *endpointError
		if errors.As(err, &ee) && (ee.status == http.StatusForbidden || ee.status == http.StatusNotFound) {
			// pending — keep polling. RFC 8628 slow_down is NOT
			// special-cased, matching the blocking flow's documented stance.
			return nil, nil
		}
		return nil, err
	}
	if pollResp.AuthorizationCode == "" || pollResp.CodeVerifier == "" {
		return nil, errors.New("codex: device approval response missing authorization_code/code_verifier (server-provided PKCE)")
	}
	return &deviceApproval{authorizationCode: pollResp.AuthorizationCode, codeVerifier: pollResp.CodeVerifier}, nil
}

// fail pins the session's terminal failure so later Polls replay it, and
// releases the coalesced waiters with the same error.
func (s *DeviceSession) fail(f *deviceFlight, err error) {
	s.mu.Lock()
	s.err = err
	if s.flight == f {
		s.flight = nil
	}
	s.mu.Unlock()
	f.approved = false
	f.err = err
	close(f.done)
}
