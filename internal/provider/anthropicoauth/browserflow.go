package anthropicoauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// LoginFromLoopback runs the same-machine browser flow: a loopback-only
// callback listener, a state-validated callback, then the code exchange.
func (a *authClient) LoginFromLoopback(ctx context.Context, cfg BrowserLogin) (LoginResult, error) {
	host, _, err := net.SplitHostPort(cfg.BindAddr)
	if err != nil {
		return LoginResult{}, fmt.Errorf("anthropicoauth: parse callback bind address %q: %w", cfg.BindAddr, err)
	}
	if host != "localhost" && !net.ParseIP(host).IsLoopback() {
		// The callback listener binds loopback only — never a wildcard.
		return LoginResult{}, fmt.Errorf("anthropicoauth: refusing non-loopback callback bind %q", cfg.BindAddr)
	}
	ln, err := net.Listen("tcp", cfg.BindAddr)
	if err != nil {
		return LoginResult{}, fmt.Errorf("anthropicoauth: bind callback listener: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	state, err := newState()
	if err != nil {
		_ = ln.Close()
		return LoginResult{}, err
	}
	authorizeURL, verifier, err := a.BuildAuthorizeURL(state, port)
	if err != nil {
		_ = ln.Close()
		return LoginResult{}, err
	}

	type callback struct{ code, state string }
	cbCh := make(chan callback, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusForbidden)
		} else {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "Login complete — you may close this window.")
		}
		select {
		case cbCh <- callback{code: q.Get("code"), state: q.Get("state")}:
		default: // a retry request must never block the handler
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	if cfg.OpenURL != nil {
		if err := cfg.OpenURL(authorizeURL); err != nil {
			return LoginResult{}, fmt.Errorf("anthropicoauth: open authorize URL: %w", err)
		}
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var cb callback
	select {
	case cb = <-cbCh:
	case <-timer.C:
		return LoginResult{}, fmt.Errorf("anthropicoauth: browser login timed out after %s", timeout)
	case <-ctx.Done():
		return LoginResult{}, ctx.Err()
	case err := <-srvErr:
		return LoginResult{}, fmt.Errorf("anthropicoauth: callback listener: %w", err)
	}
	if cb.state != state {
		return LoginResult{}, ErrStateMismatch
	}
	if cb.code == "" {
		return LoginResult{}, errors.New("anthropicoauth: callback carried no authorization code")
	}

	// The exchange redirect_uri must be byte-identical to the authorize one.
	res, err := a.exchangeCode(ctx, cb.code, state, a.redirectURI(port), verifier)
	if err != nil {
		return LoginResult{}, err
	}
	a.mu.Lock()
	a.lastRefresh = time.Now()
	a.mu.Unlock()
	return res, nil
}
