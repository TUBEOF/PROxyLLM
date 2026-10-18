package httpguard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubLookup replaces the resolver seam with fixed answers for the named
// hosts, restoring the real resolver on cleanup.
func stubLookup(hosts map[string][]net.IPAddr) func() {
	prev := lookupIPAddr
	lookupIPAddr = func(_ context.Context, host string) ([]net.IPAddr, error) {
		if addrs, ok := hosts[host]; ok {
			return addrs, nil
		}
		return nil, fmt.Errorf("no such host %s", host)
	}
	return func() { lookupIPAddr = prev }
}

// Spec: the write-path SSRF guard validates the
// operator-supplied base URL once, at config time. The runtime dial path
// must re-apply the same guarantee on every redirect: an upstream that
// answers 301/302 toward a private address (e.g. the cloud metadata
// endpoint 169.254.169.254) must not be followed, because Go re-sends
// the Authorization header on redirects — a followed redirect leaks the
// operator's Bearer key to the redirect target.

func TestNewClient_RefusesPrivateRedirect(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A hostile upstream redirecting to the cloud metadata endpoint.
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
	}))
	defer ts.Close()

	client := NewClient(0)
	resp, err := client.Get(ts.URL)
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
		t.Fatalf("redirect to private address must be refused, got status %d", resp.StatusCode)
	}
	var ge *GuardError
	if !errors.As(err, &ge) {
		t.Fatalf("refusal must be a *GuardError, got %T: %v", err, err)
	}
}

func TestNewClient_AllowsPublicRedirect(t *testing.T) {
	// httptest listeners bind loopback, so a public redirect target is
	// simulated through the resolver seam: the target host resolves to a
	// public IP and the stub answers the guard's lookup. CI must not
	// depend on live DNS or a routable public listener.
	restore := stubLookup(map[string][]net.IPAddr{
		"public-target.test": {{IP: net.ParseIP("203.0.113.7")}}, // TEST-NET-3
	})
	defer restore()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	targetURL := strings.Replace(target.URL, "127.0.0.1", "public-target.test", 1)

	// After the guard approves the redirect, the client dials the target
	// host for real — shim the dial so the simulated public host lands on
	// the httptest listener (CI must not need routable public networking).
	transport := http.DefaultTransport.(*http.Transport).Clone()
	innerDial := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if strings.HasPrefix(addr, "public-target.test:") {
			addr = strings.Replace(addr, "public-target.test", "127.0.0.1", 1)
		}
		return innerDial(ctx, network, addr)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, targetURL, http.StatusFound)
	}))
	defer ts.Close()

	client := NewClient(0)
	client.Transport = transport
	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("legitimate redirect must be followed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after redirect, got %d", resp.StatusCode)
	}
	if resp.Request.URL.Hostname() != "public-target.test" {
		t.Fatalf("expected to land on the redirect target, got %s", resp.Request.URL)
	}
}

func TestNewClient_RefusesLoopbackRedirect(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:9/inner", http.StatusMovedPermanently)
	}))
	defer ts.Close()

	client := NewClient(0)
	_, err := client.Get(ts.URL)
	if err == nil {
		t.Fatal("redirect to loopback must be refused")
	}
	var ge *GuardError
	if !errors.As(err, &ge) {
		t.Fatalf("refusal must be a *GuardError, got %T: %v", err, err)
	}
	if ge.Reason != "loopback" {
		t.Errorf("expected loopback reason, got %q", ge.Reason)
	}
}

// RED-GREEN pinned test: the streaming variant must guard redirects the
// same way — streaming is the attack surface with the longest-lived
// credentials (OAuth access tokens ride the codex transport).
func TestNewStreamingClient_RefusesPrivateRedirect(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://192.168.1.10/latest", http.StatusFound)
	}))
	defer ts.Close()

	client := NewStreamingClient()
	_, err := client.Get(ts.URL)
	if err == nil {
		t.Fatal("streaming client must refuse redirect to RFC 1918 target")
	}
	var ge *GuardError
	if !errors.As(err, &ge) {
		t.Fatalf("refusal must be a *GuardError, got %T: %v", err, err)
	}
	if ge.Reason != "RFC 1918" {
		t.Errorf("expected RFC 1918 reason, got %q", ge.Reason)
	}
}

func TestNewClient_OptOutFollowsPrivateRedirect(t *testing.T) {
	t.Setenv(OptOutEnv, "1")

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Private-by-IP: httptest binds 127.0.0.1, so a redirect to the
		// target server is a redirect to a loopback address — exactly the
		// class the opt-out must allow. Without the opt-out this redirect
		// is refused (pinned by the loopback test above).
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer ts.Close()

	client := NewClient(0)
	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("with opt-out, redirect to loopback must be followed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after opted-out redirect, got %d", resp.StatusCode)
	}
}

// The write-path guard and the redirect guard share the classification —
// pin the private/public boundary so the two consumers cannot drift.
func TestIsPrivateAddr(t *testing.T) {
	private := []string{
		"127.0.0.1", "::1", // loopback
		"10.0.0.1", "172.16.0.1", "192.168.1.1", // RFC 1918
		"169.254.169.254", "fe80::1", // link-local + metadata endpoint
		"0.0.0.0", "::", // unspecified
	}
	for _, s := range private {
		if !IsPrivateAddr(net.ParseIP(s)) {
			t.Errorf("%s must classify as private", s)
		}
	}
	public := []string{"8.8.8.8", "1.1.1.1", "2606:4700::1111"}
	for _, s := range public {
		if IsPrivateAddr(net.ParseIP(s)) {
			t.Errorf("%s must classify as public", s)
		}
	}
}

// Connection reuse across bursts. Behavioral, not a knob
// assertion: the tuned transport exists so that a burst-heavy proxy does not
// re-dial TLS for every request, and the observable fact is how many TCP
// connections a two-burst sequence opens.
//
// Sequential requests would prove nothing — http.DefaultTransport's
// MaxIdleConnsPerHost of 2 already covers a single-at-a-time walk. The case that
// distinguishes is a burst WIDER than that pool: the second burst can reuse only
// as many connections as the pool held, and dials the rest.
func TestNewClient_ReusesConnectionsAcrossBursts(t *testing.T) {
	const burst = 8

	var mu sync.Mutex
	seen := map[string]struct{}{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.RemoteAddr] = struct{}{}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := NewClient(5 * time.Second)
	oneBurst := func(n int) {
		t.Helper()
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, err := client.Get(ts.URL)
				if err != nil {
					t.Errorf("burst request: %v", err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}()
		}
		wg.Wait()
	}

	oneBurst(burst)
	oneBurst(burst)

	mu.Lock()
	defer mu.Unlock()
	// A burst of 8 needs 8 connections the first time. The second burst must ride
	// the idle pool, so the total stays at 8 (one spare allowed for timing).
	if len(seen) > burst+1 {
		t.Errorf("connections opened = %d after two bursts of %d, want <= %d — the idle pool must carry a burst wider than it",
			len(seen), burst, burst+1)
	}
}

// Spec: the shared transport's ResponseHeaderTimeout (the wall an upstream
// that accepts the connection and then says nothing hits) must be
// operator-configurable: the same 30s that rescues a hung upstream also
// turns a slow-but-correct multimodal upstream into a transport failure that
// masks the model's own capability-gap rejection (ADR-018). The option must
// change ONLY the header phase; every other tuning and the SSRF redirect
// guard stay the shared default.

func TestNewClient_HeaderTimeoutOptionOverridesTransport(t *testing.T) {
	c := NewClient(0, WithHeaderTimeout(7*time.Second))
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.Transport)
	}
	if tr.ResponseHeaderTimeout != 7*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want 7s", tr.ResponseHeaderTimeout)
	}
	// The rest of the tuning must ride the shared defaults.
	if tr.MaxIdleConnsPerHost != 32 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 32 (shared default)", tr.MaxIdleConnsPerHost)
	}
	if c.CheckRedirect == nil {
		t.Error("redirect guard missing — the option must not drop the SSRF policy")
	}
}

func TestNewClient_StreamingHeaderTimeoutOption(t *testing.T) {
	c := NewStreamingClient(WithHeaderTimeout(45 * time.Second))
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.Transport)
	}
	if tr.ResponseHeaderTimeout != 45*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want 45s", tr.ResponseHeaderTimeout)
	}
	if c.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0 (streaming keeps no total budget)", c.Timeout)
	}
}

func TestNewClient_NoOptionKeepsDefaultHeaderTimeout(t *testing.T) {
	for _, c := range []*http.Client{NewClient(0), NewStreamingClient(), NewClient(15 * time.Second)} {
		tr, ok := c.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("transport is %T, want *http.Transport", c.Transport)
		}
		if tr.ResponseHeaderTimeout != 30*time.Second {
			t.Errorf("ResponseHeaderTimeout = %v, want the 30s default", tr.ResponseHeaderTimeout)
		}
	}
}
