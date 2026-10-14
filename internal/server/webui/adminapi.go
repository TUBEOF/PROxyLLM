package webui

// Admin API: the WebUI SPA's only backend, mounted under /api/admin/.
// Session auth + CSRF are the auth layer's: login/logout/bootstrap
// delegate to auth.SessionManager, every resource handler runs behind
// RequireAdmin. Error model: uniform {"error": ...} JSON bodies (never
// distinguish failure causes to the client), ErrNotFound → 404,
// ErrInvalid → 400, everything else → 500.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tubeof/proxyllm/internal/auth"
	"github.com/tubeof/proxyllm/internal/provider/codex"
	"github.com/tubeof/proxyllm/internal/reqlog"
	"github.com/tubeof/proxyllm/internal/storage"
)

// AdminParams wires the admin API. MasterKey is the CSRF HMAC secret — the
// SAME bytes that sealed the storage envelope; the Store itself keeps only
// the *Envelope, so the caller (cmd/proxyllm) must pass the key here.
// ReloadRouting re-runs rule loading into the live router after every
// successful routing-rule write; ReloadProviders re-registers the stored
// provider set after every successful provider write: WebUI provider
// edits reach the live registry without a restart — the
// exact contract TestServer_ProviderWriteReloadsLiveRegistry pins).
// ReloadSettings re-loads the settings KV into the live runtime after every
// successful settings write (model aliases → router.SetAliases) — the DB
// override layer is live, not restart-pinned.
// Now is injectable so tests can pin the clock.
type AdminParams struct {
	Store           *storage.Store
	ReloadRouting   func()
	ReloadProviders func()
	ReloadSettings  func()
	// ReloadStrategy pushes the global routing strategy into the live
	// router after every successful routing-strategy write (ADR-009 →
	// router.SetStrategy) — same live-swap contract as ReloadSettings.
	ReloadStrategy func()
	// ReloadFallbacks pushes the capability fallback lists into the live
	// runtime after every successful fallback write (automatic capabilities:
	// vision_fallback model IDs → executor.SetCapabilityFall-
	// backs; the web-tool lists are picked up by the provider reload's pool
	// rebuild — same live-swap contract).
	ReloadFallbacks func()
	// SyncNow runs one model-sync pass synchronously (ADR-010 WebUI "Sync
	// now" button). nil when the sync is not wired (no master key).
	SyncNow func(ctx context.Context) (storage.ModelSyncState, error)
	// TestModel answers one minimal "Sag OK" round trip against the model
	// row's provider (the Models page test button). nil when not
	// wired (no master key) — the endpoint answers the uniform 404.
	TestModel func(ctx context.Context, modelID string) (TestModelResult, error)
	// HealthSnapshot names the per-account cooldown reason for the Quota
	// page badge (ADR-012; "quota_exhausted" | "breaker" | ""). nil when
	// not wired — badges render unknown.
	HealthSnapshot func(accountID string) string
	// Prober is the on-demand usage scrape (ADR-012; injectable for
	// tests). nil = the production ollama.com client.
	Prober UsageProbeFunc
	Now    func() int64

	// Stream is the live log hub (Logs-page redesign): in-flight registry
	// + finished-row broadcast. nil keeps the SSE endpoint answering 503 —
	// the page falls back to its page fetch (every existing test green).
	Stream    *reqlog.Stream
	MasterKey []byte
}

// AdminAPI serves the /api/admin/ surface. It holds no state of its own
// beyond the wired dependencies — all persistence goes through the store —
// except the OAuth device-flow registry: active flows are transient session
// state (issuer-side, single-use authorization codes), never persisted.
type AdminAPI struct {
	store           *storage.Store
	reloadRouting   func()
	reloadProviders func()
	reloadSettings  func()
	reloadStrategy  func()
	reloadFallbacks func()
	syncNow         func(ctx context.Context) (storage.ModelSyncState, error)
	testModel       func(ctx context.Context, modelID string) (TestModelResult, error)
	sessions        *auth.SessionManager
	stream          *reqlog.Stream
	// streamHeartbeat is the SSE `: ping` interval (15 s in production;
	// tests pin it lower to keep the gate fast).
	streamHeartbeat time.Duration

	// Usage probing (ADR-012): healthSnapshot names the per-account
	// cooldown reason for the Quota badge; prober scrapes on demand
	// (injectable — tests stub it; nil = the fixed ollama.com client).
	// now is the injected clock.
	healthSnapshot func(accountID string) string
	prober         UsageProbeFunc
	now            func() int64

	oauthMu    sync.Mutex
	oauthFlows map[string]*codex.DeviceSession // providerID → active flow
}

// NewAdminAPI builds the admin API and its session manager (shared master
// key, shared clock).
func NewAdminAPI(p AdminParams) *AdminAPI {
	now := p.Now
	if now == nil {
		now = func() int64 { return time.Now().Unix() }
	}
	return &AdminAPI{
		store:           p.Store,
		reloadRouting:   p.ReloadRouting,
		reloadProviders: p.ReloadProviders,
		reloadSettings:  p.ReloadSettings,
		reloadStrategy:  p.ReloadStrategy,
		reloadFallbacks: p.ReloadFallbacks,
		syncNow:         p.SyncNow,
		testModel:       p.TestModel,
		sessions:        auth.NewSessionManager(p.Store, p.MasterKey, now),
		healthSnapshot:  p.HealthSnapshot,
		prober:          p.Prober,
		stream:          p.Stream,
		streamHeartbeat: logStreamHeartbeatDefault,
		now:             now,
		oauthFlows:      map[string]*codex.DeviceSession{},
	}
}

// Handler returns the mounted /api/admin/ mux. Mutating resource routes run
// behind RequireAdmin (session + CSRF layers); login/bootstrap are the only
// unauthenticated entry points.
func (a *AdminAPI) Handler() http.Handler {
	mux := http.NewServeMux()

	// Unauthenticated surface: first-run bootstrap and login.
	mux.HandleFunc("POST /api/admin/bootstrap", a.handleBootstrap)
	mux.HandleFunc("POST /api/admin/login", a.sessions.HandleLogin)
	mux.HandleFunc("POST /api/admin/logout", a.sessions.HandleLogout)

	// Resource routes — session-gated.
	mux.Handle("GET /api/admin/provider-types", a.sessions.RequireAdmin(http.HandlerFunc(a.listProviderTypes)))
	mux.Handle("GET /api/admin/provider-types/{type}", a.sessions.RequireAdmin(http.HandlerFunc(a.getProviderType)))
	mux.Handle("PUT /api/admin/provider-types/{type}/settings", a.sessions.RequireAdmin(http.HandlerFunc(a.putProviderTypeSettings)))
	mux.Handle("POST /api/admin/provider-types/{type}/accounts", a.sessions.RequireAdmin(http.HandlerFunc(a.postProviderTypeAccount)))
	mux.Handle("GET /api/admin/providers", a.sessions.RequireAdmin(http.HandlerFunc(a.listProviders)))
	mux.Handle("PUT /api/admin/providers/{id}", a.sessions.RequireAdmin(http.HandlerFunc(a.putProvider)))
	mux.Handle("DELETE /api/admin/providers/{id}", a.sessions.RequireAdmin(http.HandlerFunc(a.deleteProvider)))
	// OAuth device flow: start surfaces the operator pair
	// (user_code + verification URL); poll performs ONE issuer round trip
	// per call, throttled to the issuer interval. There is deliberately NO
	// /oauth/callback admin route — the device flow needs no inbound
	// endpoint (that is the point: it works through Docker's isolation),
	// and the browser flow's loopback listener belongs to the codex
	// package's LoginFromBrowser, not the admin API.
	mux.Handle("POST /api/admin/providers/{id}/oauth/start", a.sessions.RequireAdmin(http.HandlerFunc(a.oauthStart)))
	mux.Handle("POST /api/admin/providers/{id}/oauth/poll", a.sessions.RequireAdmin(http.HandlerFunc(a.oauthPoll)))

	// Model IDs are prefixed routing identities (model
	// identity, "ollamacloud/glm-5.3") — the {id...} wildcard captures the
	// full multi-segment ID (legacy unprefixed IDs match it too). The test
	// route puts the ID last ({...} wildcards must end the pattern).
	mux.Handle("GET /api/admin/models", a.sessions.RequireAdmin(http.HandlerFunc(a.listModels)))
	mux.Handle("PUT /api/admin/models/{id...}", a.sessions.RequireAdmin(http.HandlerFunc(a.putModel)))
	mux.Handle("DELETE /api/admin/models/{id...}", a.sessions.RequireAdmin(http.HandlerFunc(a.deleteModel)))
	// Model test: one minimal "Sag OK" round trip per model
	// row — the Models page's test button.
	mux.Handle("POST /api/admin/models/test/{id...}", a.sessions.RequireAdmin(http.HandlerFunc(a.testModelHandler)))

	mux.Handle("GET /api/admin/routing", a.sessions.RequireAdmin(http.HandlerFunc(a.listRouting)))
	mux.Handle("PUT /api/admin/routing/{id}", a.sessions.RequireAdmin(http.HandlerFunc(a.putRoutingRule)))
	mux.Handle("DELETE /api/admin/routing/{id}", a.sessions.RequireAdmin(http.HandlerFunc(a.deleteRoutingRule)))
	mux.Handle("GET /api/admin/routing-strategy", a.sessions.RequireAdmin(http.HandlerFunc(a.getRoutingStrategy)))
	mux.Handle("PUT /api/admin/routing-strategy", a.sessions.RequireAdmin(http.HandlerFunc(a.putRoutingStrategy)))

	// ADR-010: capability fallback lists + model sync.
	mux.Handle("GET /api/admin/capability-fallbacks", a.sessions.RequireAdmin(http.HandlerFunc(a.getCapabilityFallbacks)))
	mux.Handle("PUT /api/admin/capability-fallbacks", a.sessions.RequireAdmin(http.HandlerFunc(a.putCapabilityFallbacks)))
	mux.Handle("GET /api/admin/model-sync", a.sessions.RequireAdmin(http.HandlerFunc(a.getModelSync)))
	mux.Handle("PUT /api/admin/model-sync", a.sessions.RequireAdmin(http.HandlerFunc(a.putModelSync)))
	mux.Handle("POST /api/admin/model-sync/run", a.sessions.RequireAdmin(http.HandlerFunc(a.runModelSync)))

	mux.Handle("POST /api/admin/keys", a.sessions.RequireAdmin(http.HandlerFunc(a.createKey)))
	mux.Handle("GET /api/admin/keys", a.sessions.RequireAdmin(http.HandlerFunc(a.listKeys)))
	mux.Handle("POST /api/admin/keys/{id}/revoke", a.sessions.RequireAdmin(http.HandlerFunc(a.revokeKey)))

	mux.Handle("GET /api/admin/usage", a.sessions.RequireAdmin(http.HandlerFunc(a.getUsage)))
	mux.Handle("GET /api/admin/logs", a.sessions.RequireAdmin(http.HandlerFunc(a.getLogs)))
	// Captured conversation + answer for one log row: fetched on
	// demand when the drawer opens, never as part of the list — the list is
	// polled and must not carry payloads with it.
	mux.Handle("GET /api/admin/log-payload", a.sessions.RequireAdmin(http.HandlerFunc(a.getLogPayload)))
	// Live log stream (Logs-page redesign): SSE hub feed. GET — the CSRF
	// layer only guards mutations, so the native EventSource works.
	mux.Handle("GET /api/admin/logs/stream", a.sessions.RequireAdmin(http.HandlerFunc(a.streamLogs)))

	// ADR-012 Quota page: per-account usage/limit cards + on-demand scrape.
	mux.Handle("GET /api/admin/accounts/usage", a.sessions.RequireAdmin(http.HandlerFunc(a.getAccountUsage)))
	mux.Handle("POST /api/admin/accounts/usage/refresh/{id...}", a.sessions.RequireAdmin(http.HandlerFunc(a.accountsUsageRefresh)))

	// Live updates: the Dashboard's assembled snapshot —
	// one GET per poll instead of the page's multi-request mount.
	mux.Handle("GET /api/admin/overview", a.sessions.RequireAdmin(http.HandlerFunc(a.getOverview)))

	mux.Handle("GET /api/admin/settings", a.sessions.RequireAdmin(http.HandlerFunc(a.listSettings)))
	mux.Handle("PUT /api/admin/settings", a.sessions.RequireAdmin(http.HandlerFunc(a.putSetting)))
	mux.Handle("DELETE /api/admin/settings/{key}", a.sessions.RequireAdmin(http.HandlerFunc(a.deleteSetting)))

	// Account (the Shell avatar menu): /me names the session's admin (the
	// honest username the UI shows after a refresh), the password change
	// verifies the current password (Argon2id, dummy-hash timing
	// equalization like HandleLogin), then revokes every OTHER session of
	// this admin — server-side revocation; this session survives so the
	// operator is not logged out of the very tab that changed it).
	mux.Handle("GET /api/admin/me", a.sessions.RequireAdmin(http.HandlerFunc(a.getMe)))
	mux.Handle("PUT /api/admin/password", a.sessions.RequireAdmin(http.HandlerFunc(a.putPassword)))

	return mux
}

// ---------------------------------------------------------------------------
// Bootstrap
// ---------------------------------------------------------------------------

// handleBootstrap creates the first admin — and only the first. Once any
// admin exists the endpoint refuses (409): it exists solely to close the
// zero-admin window, never to add or hijack accounts. The response never
// confirms whether an admin already exists beyond the status itself.
func (a *AdminAPI) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	// Read the body ONCE here, then hand the replayed request to HandleLogin
	// below — a decoded body cannot be re-read, and bootstrap must end in a
	// real login response (cookies + CSRF token).
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10) // bounded input
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	hash, err := storage.NewArgon2Hasher().Hash(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// TOCTOU-safe: count + insert run in ONE transaction — a
	// concurrent second bootstrap loses the race and gets ErrConflict, never
	// a silent second admin row.
	if err := a.store.EnsureFirstAdmin(r.Context(), newAdminID(), req.Username, hash); err != nil {
		if errors.Is(err, storage.ErrConflict) {
			// The zero-admin window is closed. 409, never a second admin.
			writeError(w, http.StatusConflict, "bootstrap unavailable")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Bootstrap ends logged in: the operator lands directly in the UI. The
	// replayed body carries the credentials HandleLogin re-validates.
	r.Body = io.NopCloser(bytes.NewReader(raw))
	a.sessions.HandleLogin(w, r)
}

// newAdminID mints an unguessable row id for a freshly bootstrapped admin.
func newAdminID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		// A broken CSPRNG must not silently degrade ids.
		panic("webui: rand.Read failed: " + err.Error())
	}
	return "adm-" + hex.EncodeToString(raw)
}

// ---------------------------------------------------------------------------
// Account (me / password change)
// ---------------------------------------------------------------------------

// minPasswordRunes is the WebUI password policy floor (the
// handleBootstrap body's implicit floor was "" — never enforced there
// because bootstrap is the only unauthenticated surface; this one is
// session-gated and enforces it).
const minPasswordRunes = 8

// dummyArgon2PHC2 is the well-formed never-matching Argon2id PHC used to
// equalize verification timing on the reject paths (same shape the auth
// package's login dummy uses; duplicated here so the webui package does
// not export/import a test-only constant).
const dummyArgon2PHC2 = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA"

// getMe answers the session's admin identity: the username the Shell
// avatar menu renders (the honest-UI rule — never a fabricated name). The
// admin id is the ONLY identity element the session context carries; the
// store resolves the row.
func (a *AdminAPI) getMe(w http.ResponseWriter, r *http.Request) {
	username, err := a.currentAdminUsername(r)
	if errors.Is(err, storage.ErrNotFound) {
		// Dead-session state (admin row gone / no id in ctx): uniform 401,
		// the same shape putPassword answers — never a 404.
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"username": username})
}

// putPassword changes the session admin's password: current-password
// verification (Argon2id + timing equalization), then the hash update and
// the revocation of every OTHER session of this admin — stolen or
// forgotten tabs die with the old password, the requesting session stays.
func (a *AdminAPI) putPassword(w http.ResponseWriter, r *http.Request) {
	adminID := auth.AdminIDFromContext(r.Context())
	if adminID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10) // bounded input
	var req struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}

	admin, err := a.store.GetAdminByID(r.Context(), adminID)
	if errors.Is(err, storage.ErrNotFound) {
		// A session whose admin row vanished is dead state — uniform 401.
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Timing-equalized verification: EVERY reject path costs two argon2
	// verifies (the real hash + the never-matching dummy), so latency on a
	// 400 cannot distinguish "wrong current password" from "too short" —
	// a stolen-session attacker probing the endpoint must not be able to
	// confirm a password guess by latency (the same posture HandleLogin
	// has against username probes).
	tooShort := utf8.RuneCountInString(req.New) < minPasswordRunes
	verified := storage.NewArgon2Hasher().Verify(admin.PasswordHash, req.Current)
	storage.NewArgon2Hasher().Verify(dummyArgon2PHC2, req.Current)
	if tooShort || !verified {
		// Uniform "invalid request" for both — the body's message never
		// says which of the two checks failed.
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	newHash, err := storage.NewArgon2Hasher().Hash(req.New)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := a.store.UpdateAdminPassword(r.Context(), adminID, newHash); err != nil {
		writeStoreError(w, err)
		return
	}
	// Revocation: every OTHER session of this admin dies with the old
	// password. This session's own row survives.
	_, _ = a.store.DeleteOtherSessionsForAdmin(r.Context(), adminID, auth.SessionIDHashFromContext(r.Context()))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// currentAdminUsername resolves the row the session context's admin id
// names. A session whose admin row vanished is dead state — the same
// uniform 401 shape putPassword answers (never a 404: it would tell the
// caller the row is gone rather than the session no longer being valid).
func (a *AdminAPI) currentAdminUsername(r *http.Request) (string, error) {
	adminID := auth.AdminIDFromContext(r.Context())
	if adminID == "" {
		return "", storage.ErrNotFound
	}
	ad, err := a.store.GetAdminByID(r.Context(), adminID)
	if err != nil {
		return "", err
	}
	return ad.Username, nil
}

// ---------------------------------------------------------------------------
// Providers
// ---------------------------------------------------------------------------

func (a *AdminAPI) listProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := a.store.ListProviders(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// Honest empty state: [] renders as an empty table, never null.
	out := make([]providerDTO, 0, len(providers))
	for _, p := range providers {
		out = append(out, providerDTO{
			ID:     p.ID,
			Type:   p.Type,
			Name:   p.Name,
			Config: json.RawMessage(p.Config),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
}

func (a *AdminAPI) putProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Type   string          `json:"type"`
		Name   string          `json:"name"`
		Config json.RawMessage `json:"config"`
		// APIKey is the optional top-level credential field. The provider
		// config must never carry it (the storage layer rejects
		// secret-shaped config keys); the admin
		// API routes it into the encrypted token store instead (kind
		// "access_token", the same row the device flow persists).
		APIKey string `json:"api_key"`
	}
	// Strict decode: unknown top-level fields reject the save — a typo'd
	// "apiKey" must fail loudly, not silently drop the credential (same
	// rule the config parser enforces for its own fields).
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	// The config blob must be valid JSON before it reaches the storage
	// layer's secret-key walk.
	if len(req.Config) > 0 && !json.Valid(req.Config) {
		writeError(w, http.StatusBadRequest, "config is not valid JSON")
		return
	}
	// Canonical provider types + URL/issuer validation incl. the
	// private-address SSRF guard.
	if _, err := ParseProviderConfig(req.Type, req.Config); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.store.PutProvider(r.Context(), storage.Provider{
		ID:     id,
		Type:   req.Type,
		Name:   req.Name,
		Config: req.Config,
	}); err != nil {
		writeStoreError(w, err)
		return
	}
	// The optional credential lands in the encrypted token store, not the
	// config (rejectSecretConfig covers the config path; this covers the
	// legitimate save path). An empty api_key is a no-op — the SPA sends
	// it only when the operator typed one.
	if req.APIKey != "" {
		if err := a.store.PutProviderTokens(r.Context(), storage.ProviderToken{
			ProviderID: id,
			Kind:       "access_token",
			Secret:     []byte(req.APIKey),
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	// The reload runs only after a successful write (the ReloadRouting
	// contract): the live registry never sees a set that is not the
	// persisted one.
	if a.reloadProviders != nil {
		a.reloadProviders()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *AdminAPI) deleteProvider(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteProvider(r.Context(), r.PathValue("id")); err != nil {
		writeStoreError(w, err)
		return
	}
	// A deleted provider's active flow dies with it — the SPA cannot poll a
	// ghost.
	a.oauthMu.Lock()
	delete(a.oauthFlows, r.PathValue("id"))
	a.oauthMu.Unlock()
	// DeleteProvider cascades models + tokens, so the live registry must
	// drop the provider too (deletes take effect without
	// a restart).
	if a.reloadProviders != nil {
		a.reloadProviders()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// OAuth device flow
// ---------------------------------------------------------------------------

// oauthFlowTimeout is the hard cap for one device login (the CLI allows 15 min).
const oauthFlowTimeout = 15 * time.Minute

// oauthStart begins (or restarts) the device login for a codex-oauth
// provider: it surfaces the pair the operator needs — user_code and the
// verification URL — plus the issuer-provided poll interval the SPA paces
// itself to. A second start replaces any active flow: the SPA restarts the
// sign-in after an abandoned session, and the old user_code is dead either
// way (issuer-side, single-use).
//
// The flow state lives in memory only: it is issuer-side session state (a
// pending authorization), never a secret worth persisting, and the resulting
// tokens ride the encrypted provider_tokens rows via the codex client's own
// persistence path.
func (a *AdminAPI) oauthStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	provider, err := a.store.GetProvider(r.Context(), id)
	if err != nil {
		writeStoreError(w, err) // missing row → 404, like every other resource
		return
	}
	if provider.Type != "codex-oauth" {
		// Only the codex OAuth provider has a flow; anything else is a client
		// error, never a generic 500.
		writeError(w, http.StatusBadRequest, "oauth flow requires a codex-oauth provider")
		return
	}
	var cfg struct {
		Issuer   string `json:"issuer"`
		ClientID string `json:"client_id"`
	}
	if len(provider.Config) > 0 {
		if err := json.Unmarshal(provider.Config, &cfg); err != nil {
			writeError(w, http.StatusBadRequest, "provider config is not valid JSON")
			return
		}
	}
	if cfg.Issuer == "" {
		// ADR-009 decision 1: draft accounts created through the type
		// surface carry no own fields — the issuer comes from the type
		// settings instead. This is the same overlay construction applies
		// (server/providers.go), not a second config source.
		settings, err := a.store.GetProviderTypeSettings(r.Context())
		if err == nil && len(settings[provider.Type]) > 0 {
			var shared struct {
				Issuer string `json:"issuer"`
			}
			if json.Unmarshal(settings[provider.Type], &shared) == nil {
				cfg.Issuer = shared.Issuer
			}
		}
	}
	if cfg.Issuer == "" {
		writeError(w, http.StatusBadRequest, "codex-oauth provider config needs an issuer (set the type's issuer in its settings)")
		return
	}
	client := codex.NewAuthClient(codex.AuthConfig{
		Issuer:     cfg.Issuer,
		ClientID:   cfg.ClientID, // empty → pinned default
		Store:      a.store,      // *storage.Store satisfies codex.TokenStore
		ProviderID: id,
	})
	sess, err := client.StartDeviceLogin(r.Context(), oauthFlowTimeout)
	if err != nil {
		var disabled *codex.DeviceLoginDisabledError
		if errors.As(err, &disabled) {
			// 404 on the usercode request = device login disabled — an
			// operator action, not a retry candidate.
			writeError(w, http.StatusBadRequest, "device login is not available for this issuer")
			return
		}
		writeError(w, http.StatusBadGateway, "issuer request failed")
		return
	}
	// Replace semantics: at most one active flow per provider.
	a.oauthMu.Lock()
	a.oauthFlows[id] = sess
	a.oauthMu.Unlock()
	writeJSON(w, http.StatusOK, oauthStartDTO{
		UserCode:        sess.UserCode,
		VerificationURL: sess.VerificationURL,
		IntervalSeconds: int(sess.Interval / time.Second),
	})
}

// oauthPoll drives one issuer round trip of the active flow (the codex
// session throttles to the issuer interval itself). Outcomes:
//   - pending  → 200 {"status":"pending"} — the SPA keeps polling
//   - approved → 200 {"status":"approved","account_id":...} — the token
//     triple is already exchanged AND persisted by the time this returns
//   - terminal (fatal issuer failure, timeout) → 502 uniform error; the
//     flow is dropped and the SPA must start a fresh one
//
// A poll with no active flow — never started, already approved (single-use
// code), or failed — is uniformly 400: the SPA starts a flow first.
func (a *AdminAPI) oauthPoll(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	a.oauthMu.Lock()
	sess := a.oauthFlows[id]
	a.oauthMu.Unlock()
	if sess == nil {
		writeError(w, http.StatusBadRequest, "no active oauth flow — start first")
		return
	}

	approved, res, err := sess.Poll(r.Context())
	if err != nil {
		// Terminal: the session pins the failure; drop it so a restart is not
		// blocked by a dead session.
		a.oauthMu.Lock()
		delete(a.oauthFlows, id)
		a.oauthMu.Unlock()
		writeError(w, http.StatusBadGateway, "oauth flow failed")
		return
	}
	if !approved {
		writeJSON(w, http.StatusOK, oauthPollDTO{Status: "pending"})
		return
	}
	// Approved — the flow is consumed. Remove it so the next sign-in must
	// start a fresh flow (the authorization code is single-use anyway).
	a.oauthMu.Lock()
	delete(a.oauthFlows, id)
	a.oauthMu.Unlock()
	writeJSON(w, http.StatusOK, oauthPollDTO{Status: "approved", AccountID: res.AccountID})
}

// ---------------------------------------------------------------------------
// Provider types (ADR-009 catalog + per-type settings + accounts)
// ---------------------------------------------------------------------------

// listProviderTypes serves the static catalog the Providers page renders
// its type cards and forms from (providertypes.go). Adding a provider type
// is a catalog entry + a parseProviderConfig case — this endpoint needs no
// change.
func (a *AdminAPI) listProviderTypes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"types": providerTypes})
}

// getProviderType serves one type's settings (provider_type_settings KV)
// and its accounts (the providers rows of that type). Unknown types are a
// 404, not an empty detail — the SPA must not render a form for a type the
// server does not know.
func (a *AdminAPI) getProviderType(w http.ResponseWriter, r *http.Request) {
	typ := r.PathValue("type")
	if _, ok := providerTypeByID(typ); !ok {
		writeError(w, http.StatusNotFound, "unknown provider type")
		return
	}
	settings, err := a.store.GetProviderTypeSettings(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	accounts, err := a.store.ListProvidersByProviderType(r.Context(), typ)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	out := make([]accountDTO, 0, len(accounts))
	for _, p := range accounts {
		dto := accountDTO{
			ID:            p.ID,
			Type:          p.Type,
			Name:          p.Name,
			HasCredential: a.hasCredential(r, p.ID),
		}
		// ADR-012: ollama-cloud accounts report the usage-cookie presence
		// (the cookie itself never leaves the store).
		if p.Type == ProviderTypeOllamaCloud {
			dto.HasUsageCookie = a.hasUsageCookie(r, p.ID)
		}
		out = append(out, dto)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"settings": settings[typ],
		"accounts": out,
	})
}

// putProviderTypeSettings validates and persists one type's shared
// settings. The per-type parse runs the write-path validation (URL shape +
// private-address SSRF guard) — type settings are provider config like any
// other. A model prefix must be unique across types: two types sharing a
// prefix would collide their model identities. A successful save reloads
// the live provider set (construction overlays type settings with account
// config, server/providers.go) and the live alias map (type aliases merge
// into the router — see server.loadModelAliases).
func (a *AdminAPI) putProviderTypeSettings(w http.ResponseWriter, r *http.Request) {
	typ := r.PathValue("type")
	if _, ok := providerTypeByID(typ); !ok {
		writeError(w, http.StatusNotFound, "unknown provider type")
		return
	}
	var req struct {
		Settings json.RawMessage `json:"settings"`
	}
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if len(req.Settings) == 0 {
		req.Settings = json.RawMessage("{}")
	}
	parsed, err := ParseProviderConfig(typ, req.Settings)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.validateModelPrefix(r.Context(), typ, parsed.ModelPrefix); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	settings, err := a.store.GetProviderTypeSettings(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	settings[typ] = req.Settings
	if err := a.store.PutProviderTypeSettings(r.Context(), settings); err != nil {
		writeStoreError(w, err)
		return
	}
	if a.reloadProviders != nil {
		a.reloadProviders()
	}
	// Type settings now carry aliases — the live alias map must fold them
	// in (same last-known-good reload contract as the KV alias write).
	if a.reloadSettings != nil {
		a.reloadSettings()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// validateModelPrefix enforces prefix uniqueness across types: a prefix is
// a model-identity namespace, and two types claiming the same one would
// collide their synced model IDs at the next sync. The type's own current
// value is exempt (an unchanged prefix must not trip its own check).
func (a *AdminAPI) validateModelPrefix(ctx context.Context, typ, prefix string) error {
	if prefix == "" {
		return nil
	}
	settings, err := a.store.GetProviderTypeSettings(ctx)
	if err != nil {
		return err
	}
	// The type string itself is reserved per type by construction — the
	// default. A custom prefix colliding with another type's STRING is a
	// future ID collision too (that type's default-prefixed models).
	for _, other := range providerTypes {
		if other.Type == typ || other.Kind != KindLLM {
			continue
		}
		if prefix == other.Type {
			return errors.New("model_prefix " + prefix + " collides with the type string of " + other.Type)
		}
	}
	for otherTyp, blob := range settings {
		if otherTyp == typ {
			continue
		}
		var otherCfg struct {
			ModelPrefix string `json:"model_prefix"`
		}
		if len(blob) == 0 || json.Unmarshal(blob, &otherCfg) != nil {
			continue
		}
		if otherCfg.ModelPrefix != "" && otherCfg.ModelPrefix == prefix {
			return errors.New("model_prefix " + prefix + " is already used by type " + otherTyp)
		}
	}
	return nil
}

// postProviderTypeAccount creates one account (providers row) of a type —
// the form path of the provider settings page. The server mints the ID
// (operator-supplied IDs are a footgun: they collide with type strings and
// each other); an optional api_key lands in the encrypted token store, the
// same kind "access_token" row the provider PUT and the device flow use.
// OAuth types refuse api_key — their credential arrives via the device
// flow, and a form key would be dead material.
func (a *AdminAPI) postProviderTypeAccount(w http.ResponseWriter, r *http.Request) {
	typ := r.PathValue("type")
	desc, ok := providerTypeByID(typ)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown provider type")
		return
	}
	var req struct {
		Name   string          `json:"name"`
		APIKey string          `json:"api_key"`
		Config json.RawMessage `json:"config"`
		// UsageCookie is the OPTIONAL ADR-012 scrape credential
		// (ollama-cloud accounts): the raw __Secure-session value. It
		// lands in the ENCRYPTED token store (kind usage_cookie), never
		// in the config blob, never in any log (redactor list).
		UsageCookie string `json:"usage_cookie"`
	}
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if desc.Auth == "oauth" && req.APIKey != "" {
		writeError(w, http.StatusBadRequest, "oauth accounts sign in via the device flow, not an api key")
		return
	}
	if desc.Auth == "api_key" && req.APIKey == "" && len(req.Config) == 0 {
		// An api_key type without a key is legal (the upstream answers the
		// auth failure; keyless local endpoints exist) — but it must be a
		// deliberate empty config, not a forgotten form field. Requiring
		// the explicit empty config keeps the honest-state contract.
		writeError(w, http.StatusBadRequest, "api key is required for api-key providers")
		return
	}

	id := newAccountID()
	// ADR-009 decision 1: the effective config is type settings overlaid by
	// the account's own fields (account wins). The account row stores only
	// its own blob; the merge happens at validation (here) and at
	// construction (server/providers.go) — so an oauth draft with no own
	// fields validates against the type's issuer, exactly like a row
	// created through the provider PUT with the issuer inline.
	own := req.Config
	if len(own) == 0 {
		own = json.RawMessage("{}")
	}
	if err := a.validateAccountConfig(r, typ, own); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.store.PutProvider(r.Context(), storage.Provider{
		ID: id, Type: typ, Name: req.Name, Config: own,
	}); err != nil {
		writeStoreError(w, err)
		return
	}
	if req.APIKey != "" {
		if err := a.store.PutProviderTokens(r.Context(), storage.ProviderToken{
			ProviderID: id,
			Kind:       "access_token",
			Secret:     []byte(req.APIKey),
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	// ADR-012: the usage cookie rides the same encrypted store (separate
	// kind — rotating the API key must not touch the scrape credential).
	if req.UsageCookie != "" && desc.Auth != "oauth" {
		if err := a.store.PutProviderTokens(r.Context(), storage.ProviderToken{
			ProviderID: id,
			Kind:       UsageCookieTokenKind,
			Secret:     []byte(req.UsageCookie),
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	if a.reloadProviders != nil {
		a.reloadProviders()
	}
	hasUsageCookie := req.UsageCookie != ""
	writeJSON(w, http.StatusOK, map[string]any{"account": accountDTO{
		ID:             id,
		Type:           typ,
		Name:           req.Name,
		HasCredential:  req.APIKey != "",
		HasUsageCookie: hasUsageCookie,
	}})
}

// validateAccountConfig checks an account's own config blob against the
// type's validation rules with the type settings overlaid (ADR-009
// decision 1: shared fields come from provider_type_settings, the account's
// own fields win). The account row persists ONLY its own blob; the merge is
// a validation-and-construction concern, never stored twice.
func (a *AdminAPI) validateAccountConfig(r *http.Request, typ string, own json.RawMessage) error {
	settings, err := a.store.GetProviderTypeSettings(r.Context())
	if err != nil {
		return err
	}
	merged, err := MergeProviderConfig(settings[typ], own)
	if err != nil {
		return err
	}
	_, err = ParseProviderConfig(typ, merged)
	return err
}

// hasCredential probes the encrypted token store for the account's
// access_token — display-only presence, the secret itself never leaves the
// store. Mirrors providerAPIKey's error-swallowing: a transient store
// failure degrades to "no credential" rather than failing the list.
func (a *AdminAPI) hasCredential(r *http.Request, providerID string) bool {
	_, err := a.store.GetProviderToken(r.Context(), providerID, "access_token")
	return err == nil
}

// hasUsageCookie probes the token store for the ADR-012 scrape credential
// (kind usage_cookie) — same presence-only contract as hasCredential.
func (a *AdminAPI) hasUsageCookie(r *http.Request, providerID string) bool {
	_, err := a.store.GetProviderToken(r.Context(), providerID, UsageCookieTokenKind)
	return err == nil
}

// newAccountID mints an unguessable account row id (newAdminID pattern).
func newAccountID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		// A broken CSPRNG must not silently degrade ids.
		panic("webui: rand.Read failed: " + err.Error())
	}
	return "acc-" + hex.EncodeToString(raw)
}

// ---------------------------------------------------------------------------
// Routing strategy (ADR-009 global default)
// ---------------------------------------------------------------------------

// getRoutingStrategy serves the global strategy override. The storage read
// already folds missing/invalid rows into the round_robin default.
func (a *AdminAPI) getRoutingStrategy(w http.ResponseWriter, r *http.Request) {
	strategy, err := a.store.GetRoutingStrategy(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"strategy": strategy})
}

// putRoutingStrategy persists the global strategy and pushes it into the
// live router (ReloadStrategy hook — the same live-swap contract as the
// rule reload). Invalid values are a 400 at the boundary, never a value
// the resolver would have to guess about.
func (a *AdminAPI) putRoutingStrategy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Strategy string `json:"strategy"`
	}
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := a.store.SetRoutingStrategy(r.Context(), req.Strategy); err != nil {
		writeStoreError(w, err)
		return
	}
	if a.reloadStrategy != nil {
		a.reloadStrategy()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// validateChain rejects chain entries that reference nothing (ADR-009).
// Entries name a provider type, a provider ID, or — the legacy wire syntax
// still stored by seeds and older rules — a "provider-id/model-id" pair;
// the provider part before any "/" is what must resolve. The runtime
// skip + trace stays as the safety net for accounts deleted after the
// rule was saved — but entries that never referenced anything must not
// persist.
func (a *AdminAPI) validateChain(ctx context.Context, chain []string) error {
	if len(chain) == 0 {
		return nil // the storage layer rejects empty chains its own way
	}
	providers, err := a.store.ListProviders(ctx)
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(providers))
	for _, p := range providers {
		known[p.ID] = true
	}
	for _, entry := range chain {
		providerID, _, _ := strings.Cut(entry, "/")
		if known[providerID] {
			continue
		}
		if desc, ok := providerTypeByID(entry); ok {
			// ADR-010: web_tool types are emulator backends, not chat
			// models — an LLM chain entry naming one can never route, so
			// the save refuses it instead of persisting a guaranteed skip.
			if desc.Kind == KindWebTool {
				return fmt.Errorf("chain entry %q is a web-tool provider, not routable as an LLM chain entry", entry)
			}
			continue
		}
		return fmt.Errorf("chain entry %q is not a known provider type or provider id", entry)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

func (a *AdminAPI) listModels(w http.ResponseWriter, r *http.Request) {
	models, err := a.store.ListModels(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	out := make([]modelDTO, 0, len(models))
	for _, m := range models {
		out = append(out, modelDTO{
			ID:           m.ID,
			ProviderID:   m.ProviderID,
			Name:         m.Name,
			Capabilities: json.RawMessage(m.Capabilities),
			Compat:       json.RawMessage(m.Compat),
			Priority:     m.Priority,
			Origin:       m.Origin,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out})
}

func (a *AdminAPI) putModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		ProviderID   string          `json:"provider_id"`
		Name         string          `json:"name"`
		Capabilities json.RawMessage `json:"capabilities"`
		Compat       json.RawMessage `json:"compat"`
		Priority     int             `json:"priority"`
	}
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	// The SPA never sends compat anymore (the policy is
	// derived server-side). An ABSENT field preserves the stored blob
	// (the legacy read-only override must survive operator saves); an
	// explicit field still replaces it. Presence, not value, is the signal.
	if len(req.Compat) == 0 {
		if existing, err := a.store.GetModel(r.Context(), id); err == nil {
			req.Compat = existing.Compat
		}
	}
	// FK guarantee surfaced as a deterministic 400 — never a SQLite error
	// string match (models.provider_id FK).
	if _, err := a.store.GetProvider(r.Context(), req.ProviderID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusBadRequest, "unknown provider")
			return
		}
		writeStoreError(w, err)
		return
	}
	if err := a.store.PutModel(r.Context(), storage.Model{
		ID:           id,
		ProviderID:   req.ProviderID,
		Name:         req.Name,
		Capabilities: req.Capabilities,
		Compat:       req.Compat,
		Priority:     req.Priority,
		// ADR-010: a WebUI save is operator authorship — a synced row edited
		// here becomes manual (the sync stops tracking it), a new row is
		// manual from birth. Empty origin = manual (storage default).
		Origin: storage.ModelManualOrigin,
	}); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *AdminAPI) deleteModel(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteModel(r.Context(), r.PathValue("id")); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// Routing rules
// ---------------------------------------------------------------------------

func (a *AdminAPI) listRouting(w http.ResponseWriter, r *http.Request) {
	rules, err := a.store.ListRoutingRules(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	out := make([]ruleDTO, 0, len(rules))
	for _, rule := range rules {
		out = append(out, ruleDTO{
			ID:         rule.ID,
			MatchModel: rule.MatchModel,
			Chain:      rule.Chain,
			Priority:   rule.Priority,
			Enabled:    rule.Enabled,
			Strategy:   rule.Strategy,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": out})
}

func (a *AdminAPI) putRoutingRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		MatchModel string   `json:"match_model"`
		Chain      []string `json:"chain"`
		Priority   int      `json:"priority"`
		Enabled    bool     `json:"enabled"`
		Strategy   string   `json:"strategy"`
	}
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	// Chain validation (ADR-009): every entry must be a known provider type
	// or an existing provider ID — a typo must fail the save, not silently
	// skip at request time (the same loud-typo doctrine as the strategy
	// field). Registered-at-write-time IDs may later vanish (account
	// delete); the resolver's "provider not registered" skip + trace
	// covers that runtime case.
	if err := a.validateChain(r.Context(), req.Chain); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.store.PutRoutingRule(r.Context(), storage.RoutingRule{
		ID:         id,
		MatchModel: req.MatchModel,
		Chain:      req.Chain,
		Priority:   req.Priority,
		Enabled:    req.Enabled,
		Strategy:   req.Strategy,
	}); err != nil {
		writeStoreError(w, err)
		return
	}
	// Reload only after a successful write: a rejected rule must not
	// perturb the live router (fallbacks are never silent,
	// and never half-applied either).
	if a.reloadRouting != nil {
		a.reloadRouting()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *AdminAPI) deleteRoutingRule(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteRoutingRule(r.Context(), r.PathValue("id")); err != nil {
		writeStoreError(w, err)
		return
	}
	if a.reloadRouting != nil {
		a.reloadRouting()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// Client keys
// ---------------------------------------------------------------------------

func (a *AdminAPI) createKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	// auth.CreateClientKey is THE display-once path:
	// it generates, Argon2id-hashes, persists, and returns the full key
	// exactly once. This response is the only place the secret ever exists.
	id, fullKey, err := auth.CreateClientKey(r.Context(), a.store, req.Name)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "key": fullKey})
}

func (a *AdminAPI) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := a.store.ListClientKeys(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// The DTO projects ONLY non-secret columns: no key_hash, no secret
	// half — the row is an audit record, not a key recovery surface.
	out := make([]keyDTO, 0, len(keys))
	for _, k := range keys {
		out = append(out, keyDTO{
			ID:        k.ID,
			Name:      k.Name,
			KeyPrefix: k.KeyPrefix,
			Revoked:   k.Revoked,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (a *AdminAPI) revokeKey(w http.ResponseWriter, r *http.Request) {
	if err := a.store.RevokeClientKey(r.Context(), r.PathValue("id")); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// Usage & logs
// ---------------------------------------------------------------------------

func (a *AdminAPI) getUsage(w http.ResponseWriter, r *http.Request) {
	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")
	if !isDay(from) || !isDay(to) {
		writeError(w, http.StatusBadRequest, "from/to must be YYYY-MM-DD")
		return
	}
	rows, err := a.store.QueryUsageDaily(r.Context(), from, to)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// No name projection here on purpose: the Overview page (the only
	// consumer) aggregates the rows' numbers — it renders no account or
	// key cell, so the fields would be pure response weight and the
	// projection's two config-table reads per poll pure waste. The Logs
	// page, which renders names, projects in logsToDTO (and per-wake in
	// the stream).
	out := make([]usageDTO, 0, len(rows))
	for _, u := range rows {
		out = append(out, usageDTO{
			Day:             u.Day,
			ClientKeyID:     u.ClientKeyID,
			ProviderID:      u.ProviderID,
			Model:           u.Model,
			InputTokens:     u.InputTokens,
			OutputTokens:    u.OutputTokens,
			CacheReadTokens: u.CacheReadTokens,
			Requests:        u.Requests,
			Failures:        u.Failures,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": out})
}

func (a *AdminAPI) getLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := atoiDefault(q.Get("limit"), 50)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid limit")
		return
	}
	offset, err := atoiDefault(q.Get("offset"), 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid offset")
		return
	}
	status, err := atoiDefault(q.Get("status"), 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid status")
		return
	}
	since, err := atoiDefault(q.Get("since"), 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid since")
		return
	}
	until, err := atoiDefault(q.Get("until"), 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid until")
		return
	}
	filter := storage.RequestLogFilter{
		Model:      q.Get("model"),
		ProviderID: q.Get("provider_id"),
		Status:     status,
		Since:      int64(since),
		Until:      int64(until),
	}
	// Live-feed cursor: after=<rowid> returns rows newer than
	// that id (newest-first, limit-capped) — the incremental poll the Logs
	// page uses; total is meaningless then and omitted. Additive field:
	// offset paging and the cursor can coexist.
	if afterStr := q.Get("after"); afterStr != "" {
		after, err := strconv.ParseInt(afterStr, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid after (row id)")
			return
		}
		logs, err := a.store.ListRequestLogsAfter(r.Context(), filter, limit, after)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, a.logsEnvelope(r.Context(), logs, -1))
		return
	}
	logs, total, err := a.store.ListRequestLogsPage(r.Context(), filter, limit, offset)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.logsEnvelope(r.Context(), logs, total))
}

// logPayloadDTO is the captured conversation and answer of one log row.
// found=false means nothing was stored for that row — either
// capture was off when it ran, or the row predates the feature; the UI says
// which, so an empty viewer is never mistaken for an empty request.
type logPayloadDTO struct {
	Found          bool   `json:"found"`
	CaptureEnabled bool   `json:"capture_enabled"`
	ClientRequest  string `json:"client_request,omitempty"`
	ClientResponse string `json:"client_response,omitempty"`
	Bytes          int64  `json:"bytes,omitempty"`
	Truncated      bool   `json:"truncated,omitempty"`
}

// getLogPayload serves one row's captured payload, on demand. The list endpoint
// deliberately does not carry these: it is polled every few seconds, and
// dragging prompt-sized text along with it would tax every page view to serve
// the one row an operator opened.
func (a *AdminAPI) getLogPayload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	owner := q.Get("owner")
	if owner != storage.PayloadOwnerRequest && owner != storage.PayloadOwnerChild {
		writeError(w, http.StatusBadRequest, "invalid owner (request|child)")
		return
	}
	id, err := atoiDefault(q.Get("id"), 0)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	out := logPayloadDTO{CaptureEnabled: a.payloadCaptureEnabled(r.Context())}
	p, found, err := a.store.GetRequestPayload(r.Context(), owner, int64(id))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, out)
		return
	}
	out.Found = true
	out.ClientRequest = p.ClientRequest
	out.ClientResponse = p.ClientResponse
	out.Bytes = p.Bytes
	out.Truncated = p.Truncated
	writeJSON(w, http.StatusOK, out)
}

// payloadCaptureEnabled mirrors the request-path switch, so the drawer can tell
// "capture is off" from "this row captured nothing".
func (a *AdminAPI) payloadCaptureEnabled(ctx context.Context) bool {
	v, ok, err := a.store.GetConfigValue(ctx, storage.RequestPayloadLoggingKey)
	if err != nil || !ok {
		return true
	}
	return strings.TrimSpace(v) != "0"
}

// logsEnvelope builds the getLogs response: the projected rows plus their
// sub-operations (children) keyed by parent row id, in one batched read —
// never a query per row. total < 0 omits the field (the after-cursor feed
// has no meaningful total). children is omitted entirely when no row in the
// page has any, so a page without sub-operations is byte-identical to the
// pre-children shape.
func (a *AdminAPI) logsEnvelope(ctx context.Context, logs []storage.RequestLog, total int) map[string]any {
	// One projection serves both the rows and their sub-operations. It is
	// re-read per response so an account renamed or deleted while the
	// stream is open is reflected on the next page fetch.
	names := a.logNameProjection(ctx)
	out := map[string]any{"logs": logsToDTO(logs, names)}
	if total >= 0 {
		out["total"] = total
	}
	// A failed children read must not look like "no sub-operations" — that
	// is exactly the silent degradation the feature exists to prevent. The
	// failure is explicit on the wire, so the WebUI can say so instead of
	// showing a cleaned-up route.
	byParent, childErr := a.rawLogChildren(ctx, logs)
	kids, failed := logChildrenField(byParent, childErr, indexLogRows(out["logs"]), names)
	if failed {
		out["children_unavailable"] = true
	}
	if kids != nil {
		out["children"] = kids
	}
	return out
}

// indexLogRows keys the already-projected rows by id, so a sub-operation can
// read what it inherits from the request it belongs to without a second query.
func indexLogRows(rows any) map[int64]logDTO {
	list, _ := rows.([]logDTO)
	out := make(map[int64]logDTO, len(list))
	for _, r := range list {
		out[r.ID] = r
	}
	return out
}

// logChildrenField is the single decision point for the `children` field:
// an absent map (no children anywhere) yields no field and no failure; a
// successful read projects; a read error yields no field and failed=true.
// Pure, so the three states are unit-testable without a store.
func logChildrenField(byParent map[int64][]storage.ChildLog, err error, parents map[int64]logDTO, names logNames) (field map[string][]childLogDTO, failed bool) {
	if err != nil {
		return nil, true
	}
	if len(byParent) == 0 {
		return nil, false
	}
	out := make(map[string][]childLogDTO, len(byParent))
	for parentID, ops := range byParent {
		// A step of a request carries the request's own facts: which client key
		// opened it and on which surface. Inheriting them here rather than
		// storing them per step keeps one source of truth, and it works for
		// rows written before this existed.
		parent := parents[parentID]
		dtos := make([]childLogDTO, 0, len(ops))
		for _, op := range ops {
			dtos = append(dtos, childLogDTO{
				ID:          op.ID,
				ParentID:    op.ParentID,
				TS:          op.TS,
				Kind:        op.Kind,
				ProviderID:  op.ProviderID,
				Model:       op.Model,
				FromRef:     op.FromRef,
				ToRef:       op.ToRef,
				Detail:      op.Detail,
				LatencyMS:   op.LatencyMS,
				Usage:       op.Usage,
				Protocol:    parent.Protocol,
				ClientKeyID: parent.ClientKeyID,
				KeyName:     parent.KeyName,
				KeyPrefix:   parent.KeyPrefix,
				// A failover's from/to are account ids in the trace; the
				// operator needs the account names. A ref that is not a
				// known account (a model id, or a deleted row) passes
				// through unchanged.
				ProviderName: names.optionalName(op.ProviderID),
				ProviderKind: names.optionalKind(op.ProviderID),
				FromName:     names.optionalName(op.FromRef),
				ToName:       names.optionalName(op.ToRef),
			})
		}
		out[strconv.FormatInt(parentID, 10)] = dtos
	}
	return out, false
}

// rawLogChildren returns each page row's sub-operations keyed by parent id,
// or the store error. An empty page issues no query.
func (a *AdminAPI) rawLogChildren(ctx context.Context, logs []storage.RequestLog) (map[int64][]storage.ChildLog, error) {
	if len(logs) == 0 {
		return nil, nil
	}
	ids := make([]int64, 0, len(logs))
	for _, l := range logs {
		ids = append(ids, l.ID)
	}
	return a.store.ListChildLogsForParents(ctx, ids)
}

// childLogDTO is one sub-operation of a request (Logs page):
// a failover, a capability (vision) fallback walk, or an emulated web-tool
// call, carried beside its parent row.
type childLogDTO struct {
	ID         int64  `json:"id"`
	ParentID   int64  `json:"parent_id"`
	TS         int64  `json:"ts"`
	Kind       string `json:"kind"`
	ProviderID string `json:"provider_id,omitempty"`
	Model      string `json:"model,omitempty"`
	FromRef    string `json:"from_ref,omitempty"`
	ToRef      string `json:"to_ref,omitempty"`
	Detail     string `json:"detail,omitempty"`
	// Display projections of the raw refs above (a failover's from/to name
	// accounts, not opaque ids) — raw-value fallback, never blank.
	ProviderName string `json:"provider_name,omitempty"`
	ProviderKind string `json:"provider_kind,omitempty"`
	FromName     string `json:"from_name,omitempty"`
	ToName       string `json:"to_name,omitempty"`

	// What the step cost, when it was a step that cost something. Tokens ride
	// the same JSON object the request row carries, so the WebUI renders one
	// shape for both; latency is milliseconds. A transition has neither.
	LatencyMS int64           `json:"latency_ms,omitempty"`
	Usage     json.RawMessage `json:"usage,omitempty"`

	// Inherited from the request this step belongs to: the client key that
	// opened it and the surface it arrived on. They are the request's facts,
	// not the step's, so they are projected from the parent row instead of
	// being stored again on every step.
	Protocol    string `json:"protocol,omitempty"`
	ClientKeyID string `json:"client_key_id,omitempty"`
	KeyName     string `json:"key_name,omitempty"`
	KeyPrefix   string `json:"key_prefix,omitempty"`
}

// logsToDTO projects request-log rows (the shared shape of both reads).
// Display names ride the batch projection — deleted rows fall back to the
// raw id, never blank.
func logsToDTO(logs []storage.RequestLog, names logNames) []logDTO {
	out := make([]logDTO, 0, len(logs))
	for _, l := range logs {
		keyName, keyPrefix := names.keyDisplay(l.ClientKeyID)
		out = append(out, logDTO{
			ID:             l.ID,
			Timestamp:      l.Timestamp,
			ClientKeyID:    l.ClientKeyID,
			ProviderID:     l.ProviderID,
			Model:          l.Model,
			Status:         l.Status,
			LatencyMS:      l.LatencyMS,
			Usage:          json.RawMessage(l.Usage),
			RouteTrace:     json.RawMessage(l.RouteTrace),
			ConversationID: l.ConversationID,
			Protocol:       l.Protocol,
			Features:       json.RawMessage(l.Features),
			ErrorRedacted:  l.ErrorRedacted,
			KeyName:        keyName,
			KeyPrefix:      keyPrefix,
			ProviderName:   names.providerDisplay(l.ProviderID),
			ProviderKind:   names.providerKind(l.ProviderID),
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// Settings (config KV overrides — the top config layer)
// ---------------------------------------------------------------------------

func (a *AdminAPI) listSettings(w http.ResponseWriter, r *http.Request) {
	entries, err := a.store.ListConfigValues(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	out := make([]entryDTO, 0, len(entries))
	for _, e := range entries {
		out = append(out, entryDTO{Key: e.Key, Value: e.Value})
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}

func (a *AdminAPI) putSetting(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Key == "" {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := a.store.SetConfigValue(r.Context(), req.Key, req.Value); err != nil {
		writeStoreError(w, err)
		return
	}
	// Reload only after a successful write — the live runtime never sees a
	// value that is not the persisted one (ReloadRouting contract).
	if a.reloadSettings != nil {
		a.reloadSettings()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *AdminAPI) deleteSetting(w http.ResponseWriter, r *http.Request) {
	// Idempotent by contract (configkv.go): deleting a missing key is a
	// successful no-op.
	if err := a.store.DeleteConfigValue(r.Context(), r.PathValue("key")); err != nil {
		writeStoreError(w, err)
		return
	}
	// Same live-swap contract as putSetting.
	if a.reloadSettings != nil {
		a.reloadSettings()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// Shared plumbing
// ---------------------------------------------------------------------------

// providerDTO is the wire shape of one provider row. Config rides as raw
// JSON so per-type settings round-trip without the API knowing their schema.
type providerDTO struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Name   string          `json:"name"`
	Config json.RawMessage `json:"config"`
}

// oauthStartDTO is the operator pair the SPA renders during sign-in:
// the code to enter on the verification page, that page's URL, and the poll
// pace the issuer prescribed. It carries no token material.
type oauthStartDTO struct {
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	IntervalSeconds int    `json:"interval_seconds"`
}

// oauthPollDTO is one poll outcome: pending (keep polling at the issuer
// pace) or approved (tokens exchanged and persisted; account_id from the
// id_token claim). No token material ever rides this body.
type oauthPollDTO struct {
	Status    string `json:"status"`
	AccountID string `json:"account_id,omitempty"`
}

// modelDTO carries the per-model compat blob verbatim — the
// "Models & Compatibility" editor's persistence backend. Origin is the
// ADR-010 provenance badge ("manual" | "synced").
type modelDTO struct {
	ID           string          `json:"id"`
	ProviderID   string          `json:"provider_id"`
	Name         string          `json:"name"`
	Capabilities json.RawMessage `json:"capabilities"`
	Compat       json.RawMessage `json:"compat"`
	Priority     int             `json:"priority"`
	Origin       string          `json:"origin"`
}

type ruleDTO struct {
	ID         string   `json:"id"`
	MatchModel string   `json:"match_model"`
	Chain      []string `json:"chain"`
	Priority   int      `json:"priority"`
	Enabled    bool     `json:"enabled"`
	// Strategy is the per-rule strategy (ADR-009): "" inherits the global
	// default, otherwise round_robin or fill_first.
	Strategy string `json:"strategy"`
}

// accountDTO is one account of a provider type (ADR-009): a providers row
// projected WITHOUT any secret material — HasCredential is a presence
// probe of the encrypted token store, the secret itself never leaves it.
type accountDTO struct {
	ID            string `json:"id"`
	Type          string `json:"type"`
	Name          string `json:"name"`
	HasCredential bool   `json:"has_credential"`
	// HasUsageCookie: the OPTIONAL ADR-012 scrape credential is present
	// (ollama-cloud accounts only; the cookie itself never leaves the
	// encrypted token store). omitempty keeps the pre-ADR-012 wire shape
	// for types without the field.
	HasUsageCookie bool `json:"has_usage_cookie,omitempty"`
}

type keyDTO struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	KeyPrefix string `json:"key_prefix"`
	Revoked   bool   `json:"revoked"`
}

type usageDTO struct {
	Day             string `json:"day"`
	ClientKeyID     string `json:"client_key_id"`
	ProviderID      string `json:"provider_id"`
	Model           string `json:"model"`
	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	CacheReadTokens int64  `json:"cache_read_tokens"`
	Requests        int64  `json:"requests"`
	Failures        int64  `json:"failures"`
}

type logDTO struct {
	ID          int64           `json:"id"`
	Timestamp   int64           `json:"timestamp"`
	ClientKeyID string          `json:"client_key_id"`
	ProviderID  string          `json:"provider_id"`
	Model       string          `json:"model"`
	Status      int             `json:"status"`
	LatencyMS   int64           `json:"latency_ms"`
	Usage       json.RawMessage `json:"usage"`
	RouteTrace  json.RawMessage `json:"route_trace"`
	// Logs-page redesign (additive, omitempty: pre-patch rows omit all).
	ConversationID string          `json:"conversation_id,omitempty"`
	Protocol       string          `json:"protocol,omitempty"`
	Features       json.RawMessage `json:"features,omitempty"`
	ErrorRedacted  string          `json:"error_redacted,omitempty"`
	// Display projections (getLogs fills them; the raw ids remain).
	KeyName      string `json:"key_name,omitempty"`
	KeyPrefix    string `json:"key_prefix,omitempty"`
	ProviderName string `json:"provider_name,omitempty"`
	// ProviderKind is the provider TYPE's human label (e.g. "Ollama Cloud",
	// "ChatGPT / Codex") — what the PROVIDER column shows. The raw
	// provider_id below stays the identity/fallback.
	ProviderKind string `json:"provider_kind,omitempty"`
}

// logNames carries the batch-projected display names for one getLogs
// response (one ListClientKeys + one ListProviders per request — small
// config tables; no SQL JOIN keeps storage display-agnostic).
type logNames struct {
	keys      map[string]storage.ClientKey
	providers map[string]storage.Provider
}

// logNameProjection builds the display maps for a log page.
func (a *AdminAPI) logNameProjection(ctx context.Context) logNames {
	names := logNames{keys: map[string]storage.ClientKey{}, providers: map[string]storage.Provider{}}
	if keys, err := a.store.ListClientKeys(ctx); err == nil {
		for _, k := range keys {
			names.keys[k.ID] = k
		}
	}
	if providers, err := a.store.ListProviders(ctx); err == nil {
		for _, p := range providers {
			names.providers[p.ID] = p
		}
	}
	return names
}

// keyDisplay renders the API-key cell: "name (prefix****)" — raw id
// fallback when the key row was deleted (never blank).
func (n logNames) keyDisplay(id string) (name, prefix string) {
	if k, ok := n.keys[id]; ok {
		return k.Name, k.KeyPrefix
	}
	return id, ""
}

// providerDisplay renders provider display name (raw id fallback).
func (n logNames) providerDisplay(id string) string {
	if p, ok := n.providers[id]; ok {
		return p.Name
	}
	return id
}

// providerKind renders the provider TYPE's human label (e.g. "Ollama
// Cloud") — the PROVIDER column's value. Falls back to the raw id: a
// deleted account, or a type the catalog no longer knows (the catalog is
// the same source the Providers page renders from), must still say
// something an operator can act on.
func (n logNames) providerKind(id string) string {
	p, ok := n.providers[id]
	if !ok {
		return id
	}
	if desc, isType := providerTypeByID(p.Type); isType && desc.Label != "" {
		return desc.Label
	}
	return p.Type
}

// optionalName is providerDisplay for refs that may not be account ids at
// all (a trace's from/to can carry a model id): an unknown ref projects
// EMPTY so the field is omitted, rather than echoing the raw value into a
// place the operator reads as a name.
//
// A failover ref is a "provider/model" composite; the provider half is the
// account, so it is resolved and the model half dropped from the name (the
// raw ref stays on the wire for the tooltip).
func (n logNames) optionalName(ref string) string {
	if ref == "" {
		return ""
	}
	id := ref
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		id = ref[:i]
	}
	p, ok := n.providers[id]
	if !ok {
		return ""
	}
	return DisplayName(p)
}

// optionalKind is providerKind's omit-when-unknown sibling.
func (n logNames) optionalKind(id string) string {
	if id == "" {
		return ""
	}
	p, ok := n.providers[id]
	if !ok {
		return ""
	}
	if desc, isType := providerTypeByID(p.Type); isType && desc.Label != "" {
		return desc.Label
	}
	return p.Type
}

// DisplayName is optionalName's no-lookup sibling for callers that already
// hold the row: the account name, raw-id fallback.
func DisplayName(p storage.Provider) string {
	if p.Name != "" {
		return p.Name
	}
	return p.ID
}

type entryDTO struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// writeJSON emits one JSON response body.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError emits the uniform admin error shape: identical
// structure for every failure, never echoing internal details.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeStoreError maps a storage error onto the HTTP contract: missing row
// → 404, validation rejection → 400, anything else → 500. Sentinel matching
// (errors.Is) — never message-string parsing.
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, storage.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid input")
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// decodeBody parses one bounded JSON request body (every admin
// input is size-capped and must be valid JSON) with strict field checking:
// unknown fields reject the request, not get silently dropped — the same
// typo'd-field-must-fail-loudly contract putProvider already enforces. The
// admin API's only client is the bundled WebUI, which sends known shapes.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// isDay reports whether s is a well-formed YYYY-MM-DD day string — the
// usage endpoint's inclusive range bounds (storage compares lexicographically).
func isDay(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	if _, err := time.Parse("2006-01-02", s); err != nil {
		return false
	}
	return true
}

// atoiDefault parses s as an int, returning def when s is empty. A malformed
// non-empty value is an error the caller maps to 400.
func atoiDefault(s string, def int) (int, error) {
	if s == "" {
		return def, nil
	}
	return strconv.Atoi(s)
}
