package webui

// Admin API (plan §5.2): the WebUI SPA's only backend, mounted under
// /api/admin/. Session auth + CSRF are the committed auth layer's
// (Phase 2 research §10): login/logout/bootstrap delegate to
// auth.SessionManager, every resource handler runs behind RequireAdmin.
// Error model: uniform {"error": ...} JSON bodies (Analysis G: never
// distinguish failure causes to the client), ErrNotFound → 404,
// ErrInvalid → 400, everything else → 500.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/proxyllm/proxyllm/internal/auth"
	"github.com/proxyllm/proxyllm/internal/provider/codex"
	"github.com/proxyllm/proxyllm/internal/storage"
)

// AdminParams wires the admin API. MasterKey is the CSRF HMAC secret — the
// SAME bytes that sealed the storage envelope; the Store itself keeps only
// the *Envelope, so the caller (cmd/proxyllm) must pass the key here.
// ReloadRouting re-runs rule loading into the live router after every
// successful routing-rule write; ReloadProviders re-registers the stored
// provider set after every successful provider write (Task 5 Gap #2:
// WebUI provider edits reach the live registry without a restart — the
// exact contract TestServer_ProviderWriteReloadsLiveRegistry pins).
// Now is injectable so tests can pin the clock.
type AdminParams struct {
	Store           *storage.Store
	ReloadRouting   func()
	ReloadProviders func()
	Now             func() int64
	MasterKey       []byte
}

// AdminAPI serves the /api/admin/ surface. It holds no state of its own
// beyond the wired dependencies — all persistence goes through the store —
// except the OAuth device-flow registry: active flows are transient session
// state (issuer-side, single-use authorization codes), never persisted.
type AdminAPI struct {
	store           *storage.Store
	reloadRouting   func()
	reloadProviders func()
	sessions        *auth.SessionManager

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
		sessions:        auth.NewSessionManager(p.Store, p.MasterKey, now),
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
	mux.Handle("GET /api/admin/providers", a.sessions.RequireAdmin(http.HandlerFunc(a.listProviders)))
	mux.Handle("PUT /api/admin/providers/{id}", a.sessions.RequireAdmin(http.HandlerFunc(a.putProvider)))
	mux.Handle("DELETE /api/admin/providers/{id}", a.sessions.RequireAdmin(http.HandlerFunc(a.deleteProvider)))
	// OAuth device flow (X20, plan §5.2): start surfaces the operator pair
	// (user_code + verification URL); poll performs ONE issuer round trip
	// per call, throttled to the issuer interval. There is deliberately NO
	// /oauth/callback admin route — the device flow needs no inbound
	// endpoint (that is the point: it works through Docker's isolation),
	// and the browser flow's loopback listener belongs to the codex
	// package's LoginFromBrowser, not the admin API.
	mux.Handle("POST /api/admin/providers/{id}/oauth/start", a.sessions.RequireAdmin(http.HandlerFunc(a.oauthStart)))
	mux.Handle("POST /api/admin/providers/{id}/oauth/poll", a.sessions.RequireAdmin(http.HandlerFunc(a.oauthPoll)))

	mux.Handle("GET /api/admin/models", a.sessions.RequireAdmin(http.HandlerFunc(a.listModels)))
	mux.Handle("PUT /api/admin/models/{id}", a.sessions.RequireAdmin(http.HandlerFunc(a.putModel)))
	mux.Handle("DELETE /api/admin/models/{id}", a.sessions.RequireAdmin(http.HandlerFunc(a.deleteModel)))

	mux.Handle("GET /api/admin/routing", a.sessions.RequireAdmin(http.HandlerFunc(a.listRouting)))
	mux.Handle("PUT /api/admin/routing/{id}", a.sessions.RequireAdmin(http.HandlerFunc(a.putRoutingRule)))
	mux.Handle("DELETE /api/admin/routing/{id}", a.sessions.RequireAdmin(http.HandlerFunc(a.deleteRoutingRule)))

	mux.Handle("POST /api/admin/keys", a.sessions.RequireAdmin(http.HandlerFunc(a.createKey)))
	mux.Handle("GET /api/admin/keys", a.sessions.RequireAdmin(http.HandlerFunc(a.listKeys)))
	mux.Handle("POST /api/admin/keys/{id}/revoke", a.sessions.RequireAdmin(http.HandlerFunc(a.revokeKey)))

	mux.Handle("GET /api/admin/usage", a.sessions.RequireAdmin(http.HandlerFunc(a.getUsage)))
	mux.Handle("GET /api/admin/logs", a.sessions.RequireAdmin(http.HandlerFunc(a.getLogs)))

	mux.Handle("GET /api/admin/settings", a.sessions.RequireAdmin(http.HandlerFunc(a.listSettings)))
	mux.Handle("PUT /api/admin/settings", a.sessions.RequireAdmin(http.HandlerFunc(a.putSetting)))
	mux.Handle("DELETE /api/admin/settings/{key}", a.sessions.RequireAdmin(http.HandlerFunc(a.deleteSetting)))

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
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10) // Analysis G: bounded input
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
	n, err := a.store.CountAdmins(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n > 0 {
		// The zero-admin window is closed. 409, never a second admin.
		writeError(w, http.StatusConflict, "bootstrap unavailable")
		return
	}
	hash, err := storage.NewArgon2Hasher().Hash(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := a.store.CreateAdmin(r.Context(), newAdminID(), req.Username, hash); err != nil {
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
		// secret-shaped config keys — Analysis G control 2); the admin
		// API routes it into the encrypted token store instead (kind
		// "access_token", the same row the device flow persists).
		APIKey string `json:"api_key"`
	}
	if err := decodeBody(w, r, &req); err != nil {
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
	// private-address SSRF guard (Task 5 Gap #2, Analysis G control 5).
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
	// drop the provider too (Task 5 Gap #2: deletes take effect without
	// a restart).
	if a.reloadProviders != nil {
		a.reloadProviders()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// OAuth device flow (X20, plan §5.2)
// ---------------------------------------------------------------------------

// oauthFlowTimeout is the X20 hard cap for one device login (CLI: 15 min).
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
		writeError(w, http.StatusBadRequest, "codex-oauth provider config needs an issuer")
		return
	}
	client := codex.NewAuthClient(codex.AuthConfig{
		Issuer:     cfg.Issuer,
		ClientID:   cfg.ClientID, // empty → pinned default (X13)
		Store:      a.store,      // *storage.Store satisfies codex.TokenStore
		ProviderID: id,
	})
	sess, err := client.StartDeviceLogin(r.Context(), oauthFlowTimeout)
	if err != nil {
		var disabled *codex.DeviceLoginDisabledError
		if errors.As(err, &disabled) {
			// X20: 404 on the usercode request = device login disabled — an
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
	// FK guarantee surfaced as a deterministic 400 — never a SQLite error
	// string match (models.provider_id FK, plan Analysis C).
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
	}
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := a.store.PutRoutingRule(r.Context(), storage.RoutingRule{
		ID:         id,
		MatchModel: req.MatchModel,
		Chain:      req.Chain,
		Priority:   req.Priority,
		Enabled:    req.Enabled,
	}); err != nil {
		writeStoreError(w, err)
		return
	}
	// Reload only after a successful write: a rejected rule must not
	// perturb the live router (Analysis E — fallbacks are never silent,
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
	// auth.CreateClientKey is THE display-once path (Analysis G control 1):
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
	logs, total, err := a.store.ListRequestLogsPage(r.Context(), storage.RequestLogFilter{
		Model:      q.Get("model"),
		ProviderID: q.Get("provider_id"),
		Status:     status,
		Since:      int64(since),
		Until:      int64(until),
	}, limit, offset)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	out := make([]logDTO, 0, len(logs))
	for _, l := range logs {
		out = append(out, logDTO{
			ID:          l.ID,
			Timestamp:   l.Timestamp,
			ClientKeyID: l.ClientKeyID,
			ProviderID:  l.ProviderID,
			Model:       l.Model,
			Status:      l.Status,
			LatencyMS:   l.LatencyMS,
			Usage:       json.RawMessage(l.Usage),
			RouteTrace:  json.RawMessage(l.RouteTrace),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": out, "total": total})
}

// ---------------------------------------------------------------------------
// Settings (config KV overrides — Analysis C top layer)
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
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *AdminAPI) deleteSetting(w http.ResponseWriter, r *http.Request) {
	// Idempotent by contract (configkv.go): deleting a missing key is a
	// successful no-op.
	if err := a.store.DeleteConfigValue(r.Context(), r.PathValue("key")); err != nil {
		writeStoreError(w, err)
		return
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

// oauthStartDTO is the X20 operator pair the SPA renders during sign-in:
// the code to enter on the verification page, that page's URL, and the poll
// pace the issuer prescribed. It carries no token material.
type oauthStartDTO struct {
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	IntervalSeconds int    `json:"interval_seconds"`
}

// oauthPollDTO is one poll outcome: pending (keep polling at the issuer
// pace) or approved (tokens exchanged and persisted; account_id from the
// id_token claim, X15). No token material ever rides this body.
type oauthPollDTO struct {
	Status    string `json:"status"`
	AccountID string `json:"account_id,omitempty"`
}

// modelDTO carries the per-model compat blob (§6.4) verbatim — the
// "Models & Compatibility" editor's persistence backend.
type modelDTO struct {
	ID           string          `json:"id"`
	ProviderID   string          `json:"provider_id"`
	Name         string          `json:"name"`
	Capabilities json.RawMessage `json:"capabilities"`
	Compat       json.RawMessage `json:"compat"`
	Priority     int             `json:"priority"`
}

type ruleDTO struct {
	ID         string   `json:"id"`
	MatchModel string   `json:"match_model"`
	Chain      []string `json:"chain"`
	Priority   int      `json:"priority"`
	Enabled    bool     `json:"enabled"`
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

// writeError emits the uniform admin error shape (Analysis G): identical
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

// decodeBody parses one bounded JSON request body (Analysis G: every admin
// input is size-capped and must be valid JSON).
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	return json.NewDecoder(r.Body).Decode(v)
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
