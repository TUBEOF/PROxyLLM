// Package fakeidp is an in-process OAuth authorization server for tests
// (work order G7): authorize redirect → code, JSON token exchange with PKCE,
// refresh rotation and a controllable expiry. It stands in for Anthropic's
// issuer in tests that must not touch the network; MockServer still plays the
// upstream Messages API.
package fakeidp

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"time"
)

// Server is one fake issuer.
type Server struct {
	*httptest.Server
	ClientID string
	// AccessTTL is returned as expires_in (seconds); default 8h.
	AccessTTL int

	mu        sync.Mutex
	codes     map[string]string // code → PKCE challenge
	refresh   map[string]bool   // live refresh tokens
	rotations int
}

// New starts a fake issuer. Close it with Server.Close.
func New(clientID string) *Server {
	s := &Server{
		ClientID:  clientID,
		AccessTTL: 28800,
		codes:     map[string]string{},
		refresh:   map[string]bool{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// Rotations reports how many refresh grants were served.
func (s *Server) Rotations() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rotations
}

// SeedRefresh registers a live refresh token (for a test that starts from a
// stored credential).
func (s *Server) SeedRefresh(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh[token] = true
}

// AuthorizeURL is the URL a browser would open. state and challenge are the
// client's PKCE values; the redirect must carry them back.
func (s *Server) AuthorizeURL(state, challenge, redirectURI string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", s.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	return s.URL + "/authorize?" + q.Encode()
}

// Approve issues a code for the challenge and returns the redirect the
// browser would follow. Tests call this after "signing in".
func (s *Server) Approve(challenge, redirectURI, state string) string {
	code := fmt.Sprintf("code-%d", time.Now().UnixNano())
	s.mu.Lock()
	s.codes[code] = challenge
	s.mu.Unlock()
	q := url.Values{}
	q.Set("code", code)
	q.Set("state", state)
	return redirectURI + "?" + q.Encode()
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/authorize":
		q := r.URL.Query()
		if q.Get("client_id") != s.ClientID {
			http.Error(w, "client", http.StatusBadRequest)
			return
		}
		redirect := q.Get("redirect_uri")
		if redirect == "" {
			http.Error(w, "redirect", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, s.Approve(q.Get("code_challenge"), redirect, q.Get("state")), http.StatusFound)
	case "/token":
		s.token(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GrantType    string `json:"grant_type"`
		Code         string `json:"code"`
		CodeVerifier string `json:"code_verifier"`
		RefreshToken string `json:"refresh_token"`
		ClientID     string `json:"client_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "json", http.StatusBadRequest)
		return
	}
	if body.ClientID != s.ClientID {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch body.GrantType {
	case "authorization_code":
		s.mu.Lock()
		challenge, ok := s.codes[body.Code]
		delete(s.codes, body.Code)
		s.mu.Unlock()
		if !ok || challenge != pkceChallenge(body.CodeVerifier) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"bad code or verifier"}`))
			return
		}
		refresh := fmt.Sprintf("refresh-%d", time.Now().UnixNano())
		s.mu.Lock()
		s.refresh[refresh] = true
		s.mu.Unlock()
		writeTokens(w, s.AccessTTL, refresh)
	case "refresh_token":
		s.mu.Lock()
		ok := s.refresh[body.RefreshToken]
		if ok {
			delete(s.refresh, body.RefreshToken) // single-use rotation
			s.rotations++
		}
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token revoked"}`))
			return
		}
		next := fmt.Sprintf("refresh-%d", time.Now().UnixNano())
		s.mu.Lock()
		s.refresh[next] = true
		s.mu.Unlock()
		writeTokens(w, s.AccessTTL, next)
	default:
		http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
	}
}

func writeTokens(w http.ResponseWriter, ttl int, refresh string) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token_type":    "Bearer",
		"access_token":  fmt.Sprintf("access-%d", time.Now().UnixNano()),
		"refresh_token": refresh,
		"expires_in":    ttl,
		"account":       map[string]any{"email_address": "operator@example.com"},
	})
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
