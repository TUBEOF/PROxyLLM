// Package auth implements client-key authentication for both API surfaces
// (plan §5.1): Bearer keys on the OpenAI surface (O1), x-api-key and/or
// Bearer on the Anthropic surface (A14, C5 both-headers reality), with
// 256-bit display-once secrets stored as Argon2id hashes only (Analysis G
// control 1) and the ENV-gated PROXYLLM_AUTH_DISABLED opt-out.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"github.com/proxyllm/proxyllm/internal/storage"
)

// KeyPrefix is the wire prefix of every client key (§5.1: pk_<prefix>_<secret>).
const KeyPrefix = "pk_"

// secretBytes is the entropy budget of the secret segment: 256 bit (G
// control 1) → 43 unpadded base64url characters.
const secretBytes = 32

// prefixLen is the character length of the display-prefix segment: short
// enough to stay readable in the WebUI, long enough that only a handful of
// keys share one prefix.
const prefixLen = 8

// errKeyShape guards against a shape regression in GenerateClientKey; it
// cannot fire without a bug in this file.
var errKeyShape = errors.New("auth: generated key does not parse")

type clientKeyID struct{}

// GenerateClientKey mints one display-once secret: pk_<prefix>_<secret>.
func GenerateClientKey() (string, error) {
	secret := make([]byte, secretBytes)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	// prefixLen chars from keyAlphabet (no "_"): splitClientKey cuts at the
	// first "_", so a "_" inside the prefix would corrupt key parsing —
	// base64url cannot be used here.
	prefix := make([]byte, prefixLen)
	if _, err := rand.Read(prefix); err != nil {
		return "", err
	}
	for i := range prefix {
		prefix[i] = keyAlphabet[int(prefix[i])%len(keyAlphabet)]
	}
	b64 := base64.RawURLEncoding
	return KeyPrefix + string(prefix) + "_" + b64.EncodeToString(secret), nil
}

// splitClientKey parses pk_<prefix>_<secret> into prefix and secret
// segments; ok is false for anything malformed — never a verification
// candidate (fail closed).
func splitClientKey(key string) (prefix, secret string, ok bool) {
	if !strings.HasPrefix(key, KeyPrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(key, KeyPrefix)
	prefix, secret, found := strings.Cut(rest, "_")
	if !found || prefix == "" || secret == "" {
		return "", "", false
	}
	return prefix, secret, true
}

// keyAlphabet is the display-prefix alphabet: URL-safe chars minus "_" so
// the prefix can never collide with the key's "_" segment separator.
const keyAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

func hasher() *storage.Argon2Hasher { return storage.NewArgon2Hasher() }

// newKeyID mints a random row id (no secret material in it).
func newKeyID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// CreateClientKey generates a fresh key, stores hash + prefix + metadata,
// and returns (id, display-once secret). This is the only moment the secret
// exists in plaintext (G control 1).
func CreateClientKey(ctx context.Context, st *storage.Store, name string) (string, string, error) {
	return CreateClientKeyWithScopes(ctx, st, name, nil)
}

// CreateClientKeyWithScopes is CreateClientKey with a scopes pattern list
// (routing rule 1 consumes it later).
func CreateClientKeyWithScopes(ctx context.Context, st *storage.Store, name string, scopes []byte) (string, string, error) {
	key, err := GenerateClientKey()
	if err != nil {
		return "", "", err
	}
	prefix, secret, ok := splitClientKey(key)
	if !ok {
		return "", "", errKeyShape
	}
	phc, err := hasher().Hash(secret)
	if err != nil {
		return "", "", err
	}
	id, err := newKeyID()
	if err != nil {
		return "", "", err
	}
	if err := st.CreateClientKey(ctx, id, name, phc, prefix, scopes); err != nil {
		return "", "", err
	}
	return id, key, nil
}

// verifyResult is the outcome of a key check.
type verifyResult struct {
	id string // row id of the matching key, "" when not verified
}

// verifyKey authenticates one presented key: candidates narrow by display
// prefix, then the constant-time Argon2id comparison decides. On success it
// stamps last_used_at (observability only — a failed touch must not reject
// a valid key) and returns the row id.
func verifyKey(ctx context.Context, st *storage.Store, key string) verifyResult {
	prefix, secret, ok := splitClientKey(key)
	if !ok {
		return verifyResult{}
	}
	candidates, err := st.ListClientKeysByPrefix(ctx, prefix)
	if err != nil {
		return verifyResult{}
	}
	h := hasher()
	for _, c := range candidates {
		if !h.Verify(c.KeyHash, secret) {
			continue
		}
		_ = st.TouchClientKey(ctx, c.ID) // observability, not authn
		return verifyResult{id: c.ID}
	}
	return verifyResult{}
}

// VerifyClientKey reports whether one presented key authenticates. False on
// missing, malformed, revoked, or non-matching keys — never a distinct
// error, so callers can't use it as an oracle.
func VerifyClientKey(ctx context.Context, st *storage.Store, key string) bool {
	return verifyKey(ctx, st, key).id != ""
}

// Middleware gates API requests on a valid client key (§5.1).
type Middleware struct {
	store    *storage.Store
	disabled bool
}

// Option configures a Middleware.
type Option func(*Middleware)

// WithAuthDisabled mirrors the ENV opt-out PROXYLLM_AUTH_DISABLED=1. The
// loud startup warning is emitted by the server wiring that reads the ENV
// (one warning per process, not per middleware).
func WithAuthDisabled() Option {
	return func(m *Middleware) { m.disabled = true }
}

// NewMiddleware builds the auth middleware. opts may carry
// WithAuthDisabled() mirroring PROXYLLM_AUTH_DISABLED=1.
func NewMiddleware(st *storage.Store, opts ...Option) *Middleware {
	m := &Middleware{store: st}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Wrap gates next on client-key auth. Both header families are accepted on
// both surfaces (§5.1, C5): Authorization: Bearer pk_... and x-api-key: pk_...
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.disabled {
			next.ServeHTTP(w, r)
			return
		}

		res := verifyKey(r.Context(), m.store, ExtractClientKey(r))
		if res.id == "" {
			WriteUnauthorized(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), clientKeyID{}, res.id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ExtractClientKey pulls the client key from either documented header
// family (O1, A14). Bearer wins when present and well-formed; otherwise
// x-api-key. Empty when neither carries a key.
func ExtractClientKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		scheme, rest, ok := strings.Cut(auth, " ")
		if ok && strings.EqualFold(scheme, "Bearer") {
			return strings.TrimSpace(rest)
		}
	}
	return r.Header.Get("x-api-key")
}

// ClientKeyIDFromContext returns the verified key's row id, "" when the
// request is anonymous (auth disabled).
func ClientKeyIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(clientKeyID{}).(string)
	return id
}

// WriteUnauthorized emits the protocol-conform 401 body for the request's
// surface (§5.3): OpenAI error.type/code/message on OpenAI paths (O10),
// Anthropic error.type/message on Anthropic paths (A13).
func WriteUnauthorized(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	if isAnthropicSurface(r.URL.Path) {
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid or missing API key"}}`))
		return
	}
	_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`))
}

// isAnthropicSurface routes the 401 body shape by path (§5.1 surface split).
func isAnthropicSurface(path string) bool {
	return strings.HasPrefix(path, "/v1/messages")
}
