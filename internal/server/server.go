// Package server wires the live proxy: httpapi surfaces behind auth
// middleware, routing rules loaded from the store, providers from the
// caller, and the health endpoint.
package server

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tubeof/proxyllm/internal/auth"
	"github.com/tubeof/proxyllm/internal/config"
	"github.com/tubeof/proxyllm/internal/modelsync"
	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/reqlog"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/server/httpapi"
	"github.com/tubeof/proxyllm/internal/server/webui"
	"github.com/tubeof/proxyllm/internal/storage"
	embedui "github.com/tubeof/proxyllm/webui"
)

// Server is the root http.Handler for PROxyLLM.
type Server struct {
	cfg    *config.Config
	store  *storage.Store
	logger *slog.Logger
	mux    *http.ServeMux

	// syncCancel stops the model-sync scheduler on server shutdown (ADR-010;
	// nil when the sync never started — e.g. no master key, or tests opting
	// out).
	syncCancel context.CancelFunc
}

// serverOptions collects the New options.
type serverOptions struct {
	providers []provider.Provider
	emulator  routing.Emulator
	masterKey []byte
	modelSync bool
}

// Option configures New.
type Option func(*serverOptions)

// WithProvider registers one provider with the server's registry at
// assembly time (cmd/proxyllm registers the config-defined ones; tests
// register mocks).
func WithProvider(p provider.Provider) Option {
	return func(o *serverOptions) { o.providers = append(o.providers, p) }
}

// WithMasterKey mounts the admin API: the master key is the
// session/CSRF HMAC secret — the same bytes that sealed the storage
// envelope — so the WebUI backend only exists when the operator's key
// material is wired in. Without it the admin logic is unreachable: /api/admin/*
// answers with the client-key gate's uniform 401, never a weakened fallback.
func WithMasterKey(key []byte) Option {
	return func(o *serverOptions) { o.masterKey = key }
}

// WithEmulator wires the web-tool emulator into the routing executor.
// Without it, emulated web tools degrade to the
// documented "unavailable" error results — never silently absent.
func WithEmulator(emu routing.Emulator) Option {
	return func(o *serverOptions) { o.emulator = emu }
}

// WithModelSync enables the automatic model-import scheduler (ADR-010).
// Default OFF: the sync makes live upstream calls per provider and persists
// wall-clock-stamped state — exactly what a long-running proxy wants and
// what deterministic environments (test harnesses, the visual-regression
// fixture server) must not do. The manual "Sync now" admin endpoint works
// regardless of this option.
func WithModelSync() Option {
	return func(o *serverOptions) { o.modelSync = true }
}

// New assembles the live server: registry, router (rules from the store),
// executor, httpapi surfaces behind auth middleware, and the health
// endpoint. A nil logger is tolerated (slog.Default takes over).
// healthView converts the routing tracker's time-based state into the
// admin API's unix-second badge snapshot.
func healthView(st routing.HealthState) webui.HealthView {
	unixOrZero := func(t time.Time) int64 {
		if t.IsZero() {
			return 0
		}
		return t.Unix()
	}
	v := webui.HealthView{
		Reason:       st.Reason,
		QuotaUntil:   unixOrZero(st.QuotaUntil),
		BreakerUntil: unixOrZero(st.BreakerUntil),
	}
	v.Until = max(v.QuotaUntil, v.BreakerUntil)
	return v
}

func New(cfg *config.Config, store *storage.Store, logger *slog.Logger, opts ...Option) (*Server, error) {
	o := &serverOptions{}
	for _, opt := range opts {
		opt(o)
	}

	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		cfg:    cfg,
		store:  store,
		logger: logger,
		mux:    http.NewServeMux(),
	}

	// Provider registry: static providers join once; stored provider rows
	// (WebUI-edited) are constructed here and re-registered on every
	// admin-API provider write — no restart. Web-tool
	// emulators (firecrawl accounts, ADR-010) come back separately for the
	// executor pool.
	reg := provider.NewLiveRegistry()
	for _, p := range o.providers {
		reg.Register(p)
	}
	dbEmulators, err := loadAndReplaceProviders(context.Background(), store, logger, reg)
	if err != nil {
		return nil, err
	}

	// Routing rules live in the store (rule state is
	// WebUI-editable, not in-memory registration).
	rules, err := loadRoutingRules(context.Background(), store)
	if err != nil {
		return nil, err
	}

	// The alias map rides the same DB override layer (docs/configuration.md
	// layer 4, SPA key `model_aliases`): stored overrides must route, and
	// an invalid blob skips the map (log + empty), never a hard startup
	// failure — the KV also holds unrelated operator settings.
	aliases := loadModelAliases(context.Background(), store, logger)

	health := routing.NewHealthTracker()
	globalStrategy := routing.Strategy(storage.DefaultRoutingStrategy)
	if stored, err := store.GetRoutingStrategy(context.Background()); err != nil {
		// The storage read folds missing/invalid rows into the default — a
		// hard error here is a real store failure, logged + default, never
		// a startup death (the model_aliases precedent).
		logger.Error("routing strategy unreadable — defaulting to "+storage.DefaultRoutingStrategy, "error", err)
	} else {
		globalStrategy = routing.Strategy(stored)
	}
	router := routing.NewRouter(routing.Config{Rules: rules, Aliases: aliases, GlobalStrategy: globalStrategy},
		routing.WithRegistry(reg),
		routing.WithHealth(health),
		// ADR-011 implicit model routes: a models-table row routes its
		// model to the row's provider when no rule matched. The request
		// context rides WithRequestContext — a client disconnect cancels
		// the read (cancellation propagates end-to-end). A read failure
		// degrades to no-row (ok=false) — the last-known-good read
		// contract; the request then gets the honest no-route rejection,
		// never a misrouted chain.
		routing.WithModelLookup(func(ctx context.Context, modelID string) (string, bool) {
			row, err := store.GetModel(ctx, modelID)
			if err != nil || row.ProviderID == "" {
				return "", false
			}
			return row.ProviderID, true
		}),
		// The other half of ADR-014's learning.
		// The executor writes vision:false on a capability_gap and NOTHING read
		// it back, so an image in a conversation re-triggered the
		// describe-the-image rescue on every turn. A row that knows it cannot
		// see images is now skipped before dispatch; a row with no opinion is
		// permissive, and the gate never empties a chain.
		routing.WithModelVisionLookup(modelVisionLookup(store)),
	)

	// Capability fallback models (ADR-010 automatic capabilities): the
	// stored vision_fallback KV holds MODEL IDs resolved to chain entries
	// here (provider + wire name); a read failure logs and leaves the
	// fallback unset (never de-route on store noise — the model_aliases
	// precedent). Learning writes observed gaps back into the model rows.
	var capabilityFallbacks []routing.ChainEntry
	if entries, err := resolveCapabilityFallbacks(context.Background(), store, logger); err != nil {
		logger.Error("capability fallback read failed — routing without capability fallback models", "error", err)
	} else {
		capabilityFallbacks = entries
	}

	// ADR-010 web-tool emulator pool: the stored web_search/web_fetch
	// fallback ACCOUNT KVs order the pool (decision 3, consumed now); the
	// ENV emulator (o.emulator) rides the tail when set (the legacy
	// PROXYLLM_FIRECRAWL_API_KEY configuration path stays functional).
	emulatorPool := buildEmulatorPool(context.Background(), dbEmulators, o.emulator, store, logger)
	if emulatorPool == nil {
		// Loud in both directions (ADR-010): emulation itself is always on,
		// but without a backend every call degrades to the documented
		// "unavailable" result. Judged AFTER the stored accounts load — an
		// ENV-only check told operators with a DB account the opposite.
		logger.Warn("PROxyLLM: no web-tool backend is configured in this process (WebUI Providers → Firecrawl or Ollama Cloud (Web Tools), or PROXYLLM_FIRECRAWL_API_KEY); web_search/web_fetch definitions are still emulated, but every call is answered with the documented \"unavailable\" error result")
	}

	exec := routing.NewExecutor(routing.ExecutorConfig{
		Registry:            reg,
		Health:              health,
		Emulator:            emulatorPool,
		CapabilityFallbacks: capabilityFallbacks,
		Caption:             resolveCaptionConfig(context.Background(), store),
		OnCapabilityGap:     s.learnCapabilityGap,
		// The same lookup the router uses, so the executor can decide BEFORE
		// dispatch what the router can only decide about candidate order: when
		// every candidate is known blind, an image request is going to be
		// rejected, and an image whose description is already cached can skip
		// that round trip entirely.
		ModelVisionLookup: modelVisionLookup(store),
	})

	// Client-facing surfaces behind client-key auth.
	authOpts := []auth.Option{}
	if cfg.Auth.Disabled {
		authOpts = append(authOpts, auth.WithAuthDisabled())
		// Loud one-time startup warning — never silently off.
		logger.Warn("PROxyLLM: client-key auth is DISABLED (PROxyLLM_AUTH_DISABLED); every client can use the proxy without a key")
	}
	authMiddleware := auth.NewMiddleware(store, authOpts...)
	// One live log hub shared by both sides (Logs-page redesign): the
	// client-facing api registers in-flight entries + publishes finished
	// rows; the admin SSE endpoint subscribes. nil (tests) keeps both
	// disabled.
	liveLogs := reqlog.New()
	handler := httpapi.New(httpapi.Params{
		Store:    store,
		Router:   router,
		Executor: exec,
		Stream:   liveLogs,
		// Client-tool claim (see loadClientWebTools): Claude Code's WebFetch /
		// WebSearch ride as client tools, so only this claim makes the proxy
		// serve them. The settings reload refreshes it live.
		ClientWebTools: loadClientWebTools(context.Background(), store, logger),
	})
	// The proxy API mounts at /v1/ (every httpapi route lives there), so the
	// SPA can own the root: a browser hitting / gets the WebUI without a
	// client key — the WebUI session is the WebUI's auth, not the client
	// key's.
	s.mux.Handle("/v1/", authMiddleware.Wrap(handler))
	// Native Ollama surface: the client key rides the same middleware.
	s.mux.Handle("/api/chat", authMiddleware.Wrap(handler))
	s.mux.Handle("/api/tags", authMiddleware.Wrap(handler))

	// Reverse-proxy trust (docs/deployment.md): the configured CIDRs are
	// the peers whose X-Forwarded-* headers the WebUI origin check honors.
	// Parsed here so a typo'd entry fails startup loudly — and unconditionally,
	// not only when the admin API happens to be mounted.
	trustedProxies, err := auth.ParseTrustedProxies(cfg.Server.TrustedProxies)
	if err != nil {
		return nil, err
	}

	// Admin API: mounted only when the master key is wired —
	//
	// Without the master key there is no admin surface to mount — but the
	// path must still answer with the auth gate's uniform 401, never the
	// SPA's 404: a 404/401 distinction would reveal whether the admin API
	// is mounted at all (the no-degraded-fallback contract from
	// TestServer_NoMasterKeyNoAdminAPI).
	if o.masterKey != nil {
		admin := webui.NewAdminAPI(webui.AdminParams{
			Store:  store,
			Stream: liveLogs,
			// ADR-012 Quota page: the per-account cooldown-reason badge and
			// the on-demand usage scrape (fixed ollama.com host; the
			// cookie rides the encrypted token store).
			HealthSnapshot: func(id string) webui.HealthView {
				return healthView(health.State(id))
			},
			Prober: s.prodUsageProber(),
			ReloadRouting: func() {
				fresh, err := loadRoutingRules(context.Background(), store)
				if err != nil {
					// The persisted rule set is the truth; a failed reload
					// keeps the last known-good live rules rather than
					// de-routing everything on a transient store error.
					logger.Error("routing reload failed; keeping last known-good rules", "error", err)
					return
				}
				router.SetRules(fresh)
			},
			ReloadProviders: func() {
				// Same contract as ReloadRouting: only a successful read
				// swaps the live set; a transient store failure keeps the
				// last known-good providers (providers.go). The error is
				// already logged inside loadAndReplaceProviders — nothing
				// further this hook can do with it. ADR-010: the rebuild
				// also refreshes the executor's emulator pool.
				emus, err := loadAndReplaceProviders(context.Background(), store, logger, reg)
				if err != nil {
					return
				}
				// Reload the capability fallbacks on the SAME reload: a
				// provider deletion cascades its model rows away, so the
				// stored fallback list drops entries that name the account —
				// but only a re-resolve removes them from the live executor
				// list (SetCapabilityFallbacks otherwise keeps the stale
				// until-deleted entry, and the walk would Skip it on every
				// gap-exhausted request). A failed re-resolve keeps the live
				// list (last-known-good, same contract as the write path);
				// the caption walk's own registry check then skips the dead
				// account, so this is belt and braces.
				if entries, rerr := resolveCapabilityFallbacks(context.Background(), store, logger); rerr == nil {
					exec.SetCapabilityFallbacks(entries)
				}
				exec.SetEmulator(buildEmulatorPool(context.Background(), emus, nil, store, logger))
			},
			ReloadSettings: func() {
				// Settings KV writes reach the live runtime without restart:
				// only a successfully read alias map swaps in (SetAliases,
				// same last-known-good contract as SetRules above).
				router.SetAliases(loadModelAliases(context.Background(), store, logger))
				handler.SetClientWebTools(loadClientWebTools(context.Background(), store, logger))
				// The image-caption settings ride the same hook (ADR-014):
				// a prompt or timeout write applies to the next rescued
				// request, with the built-in defaults for anything unreadable.
				exec.SetCaptionConfig(resolveCaptionConfig(context.Background(), store))
			},
			ReloadStrategy: func() {
				// ADR-009: the routing_strategy KV write swaps the live
				// global strategy — same last-known-good contract: a failed
				// read keeps the live strategy.
				if stored, err := store.GetRoutingStrategy(context.Background()); err != nil {
					logger.Error("routing strategy reload failed; keeping last known-good strategy", "error", err)
				} else {
					router.SetStrategy(routing.Strategy(stored))
				}
			},
			ReloadFallbacks: func() {
				// Automatic capabilities (ADR-010): the vision_fallback KV
				// write pushes the freshly resolved MODEL entries into the
				// live executor (same last-known-good contract — a failed
				// read keeps the live fallback). The web-tool pool order
				// rides ReloadProviders (the fallback write also triggers it
				// — one write, both hooks).
				if entries, err := resolveCapabilityFallbacks(context.Background(), store, logger); err != nil {
					logger.Error("capability fallback reload failed; keeping last known-good models", "error", err)
				} else {
					exec.SetCapabilityFallbacks(entries)
				}
				emus, err := loadAndReplaceProviders(context.Background(), store, logger, reg)
				if err != nil {
					return
				}
				exec.SetEmulator(buildEmulatorPool(context.Background(), emus, nil, store, logger))
			},
			SyncNow: func(ctx context.Context) (storage.ModelSyncState, error) {
				return modelsync.New(store, reg, logger).Run(ctx)
			},
			TestModel: s.testModel(reg),
			MasterKey: o.masterKey,
			// The parsed server.trusted_proxies set (see the parse above the
			// admin mount): the session manager honors forwarded headers
			// from exactly these peers.
			TrustedProxies: trustedProxies,
		})
		s.mux.Handle("/api/admin/", admin.Handler())
	} else {
		s.mux.Handle("/api/admin/", authMiddleware.Wrap(http.NotFoundHandler()))
	}

	// The embedded SPA owns everything else: assets serve directly, deep
	// links fall back to index.html, and /v1/ + /api/ never reach it —
	// their mux patterns are more specific, and the handler excludes them
	// defensively anyway.
	s.mux.Handle("/", webui.NewSPAHandler(embeddedUI()))

	s.mux.HandleFunc("GET /healthz", s.handleHealth)

	// ADR-010 model sync: the scheduler starts with the server (one startup
	// run, then the configured cadence) and stops on context cancel. The
	// Syncer walks the live registry, so provider reloads are picked up
	// automatically. The enable switch is read per tick — the WebUI toggle
	// takes effect at the next tick boundary. Opt-in via WithModelSync
	// (default off — deterministic test/fixture assemblies must not make
	// live upstream calls or persist wall-clock state).
	//
	// gosec G118: the cancel is stored on the server for Close/shutdown to
	// invoke; the analyzer cannot see the cross-method call. A syncCancel
	// that is never called would leak one goroutine per server instance —
	// New always stores it, and tests calling New on many servers without
	// Close is the existing pattern (test servers are process-local).
	if o.masterKey != nil {
		ctx, cancel := context.WithCancel(context.Background()) //nolint:gosec // cancel stored on the server (syncCancel); called on shutdown
		s.syncCancel = cancel
		if o.modelSync {
			syncer := modelsync.New(store, reg, logger)
			go modelsync.NewScheduler(syncer, store, logger).Run(ctx)
		}
		// Retention (payloads, request/child rows, expired sessions) runs
		// whenever the admin-backed server is assembled, even if model sync
		// is off: a library embedding with a master key still needs bounded
		// tables.
		go runRetention(ctx, store, logger)
	}

	return s, nil
}

// buildEmulatorPool assembles the executor's web-tool emulator (ADR-010):
//   - no DB accounts and a fallback list that resolves to nothing → the
//     ENV-wired emulator (o.emulator; the legacy PROXYLLM_FIRECRAWL_API_KEY
//     path) or nil (the documented "unavailable" degrade state);
//   - otherwise → the EmulatorPool over the STORED web_search/web_fetch
//     fallback KVs (ADR-010 decision 3): each
//     list names ACCOUNT IDs in priority order; unknown entries skip with
//     a warn; an empty/unresolvable list keeps the registry insertion
//     order. Single-account pools behave exactly like the pre-ADR-010
//     single emulator.
//
// A read failure on the fallback KVs logs and uses insertion order (never
// de-route on store noise).
func buildEmulatorPool(ctx context.Context, accounts []NamedEmulator, envEmulator routing.Emulator, store *storage.Store, logger *slog.Logger) routing.Emulator {
	if len(accounts) == 0 && envEmulator == nil {
		return nil
	}
	if len(accounts) == 0 && envEmulator != nil {
		return envEmulator
	}
	// The ENV emulator joins both families as the pool's final entry.
	search, fetch, err := resolveWebToolFallbacks(ctx, store, logger, accounts)
	if err != nil {
		logger.Error("web-tool fallback read failed — pool in registry order", "error", err)
		search, fetch = accounts, accounts
	}
	searchEntries := namedToEmulatorEntries(search)
	fetchEntries := namedToEmulatorEntries(fetch)
	if envEmulator != nil {
		// The legacy ENV path joins both families as the final entry. No account
		// id: an env-configured emulator has no provider row to name, so the
		// trace cannot attribute an attempt to it and the per-account circuit
		// never gates it (see EmulatorPool.attemptOrder).
		searchEntries = append(searchEntries, routing.EmulatorEntry{Emulator: envEmulator})
		fetchEntries = append(fetchEntries, routing.EmulatorEntry{Emulator: envEmulator})
	}
	return routing.NewEmulatorPoolEntries(searchEntries, fetchEntries)
}

// namedToEmulatorEntries keeps the account ids: they are what the pool's trace
// events name and what its per-account circuit is keyed by. A
// position-keyed circuit would forget a cooling account as soon as the operator
// reordered the list.
func namedToEmulatorEntries(named []NamedEmulator) []routing.EmulatorEntry {
	out := make([]routing.EmulatorEntry, 0, len(named))
	for _, e := range named {
		out = append(out, routing.EmulatorEntry{ID: e.ID, Emulator: e.Emulator})
	}
	return out
}

// loadAndReplaceProviders builds the provider set from the store and swaps
// it into the live registry's dynamic slot; the constructed web-tool
// emulators (firecrawl accounts, ADR-010) are returned with their account
// IDs for the executor's emulator pool. On a store failure the live set is
// kept (last known-good) and the error is logged — the contract mirrors
// ReloadRouting.
func loadAndReplaceProviders(ctx context.Context, store *storage.Store, logger *slog.Logger, reg *provider.LiveRegistry) ([]NamedEmulator, error) {
	fresh, emus, err := loadProviders(ctx, store, logger)
	if err != nil {
		logger.Error("provider reload failed; keeping last known-good providers", "error", err)
		return nil, err
	}
	reg.ReplaceDynamics(fresh)
	return emus, nil
}

// loadClientWebTools reads the `client_web_tools` settings blob: the
// client-tool → emulated-family mapping the Anthropic surface claims. A
// missing key keeps the shipped default (Claude Code's WebFetch/WebSearch —
// the client names are a wire fact of the client, so the operator can override
// them without a release); an unreadable blob degrades to the default with a
// loud log, never a startup death. An explicit empty object disables the claim.
func loadClientWebTools(ctx context.Context, store *storage.Store, logger *slog.Logger) map[string]string {
	raw, ok, err := store.GetConfigValue(ctx, "client_web_tools")
	if err != nil {
		logger.Error("client_web_tools unreadable; claiming the default client web tools", "error", err)
		return httpapi.DefaultClientWebTools()
	}
	if !ok || strings.TrimSpace(raw) == "" {
		return httpapi.DefaultClientWebTools()
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		logger.Error("client_web_tools is not a JSON object; claiming the default client web tools", "error", err)
		return httpapi.DefaultClientWebTools()
	}
	return m // possibly empty: the operator turned the claim off
}

// loadModelAliases reads the SPA's `model_aliases` settings blob (JSON
// object alias→model-id) from the settings KV, then folds in every
// provider TYPE's aliases from the shared type settings (type
// settings may carry an "aliases" map — the per-type editor writes it).
// The KV entry wins on collision: it stays the operator's global escape
// hatch over the per-type UI. An absent key is the normal state (empty
// map, nil error). An invalid blob never fails the server: it logs and
// contributes nothing — the KV also holds unrelated operator settings,
// and defaults never silently route (the map stays empty until a valid
// blob is written).
func loadModelAliases(ctx context.Context, store *storage.Store, logger *slog.Logger) map[string]string {
	clean := map[string]string{}

	// Layer 1: the per-type aliases (shared type settings). Read errors and
	// malformed blobs degrade to contribution-less — never a startup death.
	typeSettings, err := store.GetProviderTypeSettings(ctx)
	if err != nil {
		logger.Error("provider type settings unreadable; routing without type aliases", "error", err)
	} else {
		for typ, blob := range typeSettings {
			var cfg struct {
				Aliases map[string]string `json:"aliases"`
			}
			if len(blob) == 0 || json.Unmarshal(blob, &cfg) != nil {
				continue
			}
			for alias, target := range cfg.Aliases {
				if strings.TrimSpace(alias) == "" || strings.TrimSpace(target) == "" {
					logger.Error("type alias entry invalid; entry skipped (fix it in the WebUI)",
						"type", typ, "alias", alias, "target", target)
					continue
				}
				clean[alias] = target
			}
		}
	}

	// Layer 2: the KV map overlays — the operator's global escape hatch.
	raw, ok, err := store.GetConfigValue(ctx, "model_aliases")
	if err != nil {
		logger.Error("model_aliases read failed; routing without KV aliases", "error", err)
		return clean
	}
	if ok && strings.TrimSpace(raw) != "" {
		var parsed map[string]string
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			logger.Error("model_aliases blob invalid; KV aliases skipped (fix it in the WebUI)", "error", err)
		} else {
			for alias, target := range parsed {
				if strings.TrimSpace(alias) == "" || strings.TrimSpace(target) == "" {
					logger.Error("model_aliases entry invalid; entry skipped (fix it in the WebUI)", "alias", alias, "target", target)
					continue
				}
				clean[alias] = target
			}
		}
	}
	return clean
}

// loadRoutingRules converts stored rules to router rules, skipping
// disabled ones (they must not participate in resolution). ID and Strategy
// ride along (ADR-009): the ID keys the round-robin rotation counters, the
// per-rule strategy overrides the global default at resolve time.
func loadRoutingRules(ctx context.Context, store *storage.Store) ([]routing.Rule, error) {
	stored, err := store.ListRoutingRules(ctx)
	if err != nil {
		return nil, err
	}
	rules := make([]routing.Rule, 0, len(stored))
	for _, r := range stored {
		if !r.Enabled {
			continue
		}
		rules = append(rules, routing.Rule{
			ID:         r.ID,
			MatchModel: r.MatchModel,
			Chain:      r.Chain,
			Priority:   r.Priority,
			Enabled:    true,
			Strategy:   routing.Strategy(r.Strategy),
		})
	}
	return rules, nil
}

// embeddedUI projects the embedded dist FS down to its root so the SPA
// handler's fs.Stat("index.html") resolves (embed.FS roots everything under
// the dist/ prefix).
func embeddedUI() fs.FS {
	sub, err := fs.Sub(embedui.Dist, "dist")
	if err != nil {
		// Cannot happen: dist is a committed directory with a placeholder
		// index.html — the embed directive fails the build without it.
		panic("webui: embedded dist is missing: " + err.Error())
	}
	return sub
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// handleHealth serves GET /healthz, the endpoint the container healthcheck
// probes. It touches the store: a process whose database is gone is not a
// healthy process, even if its HTTP listener still answers.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	w.Header().Set("Content-Type", "application/json")
	if err := s.store.Ping(ctx); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"unavailable"}`))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
