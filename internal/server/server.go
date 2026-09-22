// Package server wires the live proxy: httpapi surfaces behind auth
// middleware, routing rules loaded from the store, providers from the
// caller, and the health endpoint (plan §4.1/§5.1, Analysis G control 1).
package server

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tubeof/proxyllm/internal/auth"
	"github.com/tubeof/proxyllm/internal/config"
	"github.com/tubeof/proxyllm/internal/modelsync"
	"github.com/tubeof/proxyllm/internal/provider"
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

// WithMasterKey mounts the admin API (plan §5.2): the master key is the
// session/CSRF HMAC secret — the same bytes that sealed the storage
// envelope — so the WebUI backend only exists when the operator's key
// material is wired in. Without it the admin logic is unreachable: /api/admin/*
// answers with the client-key gate's uniform 401, never a weakened fallback.
func WithMasterKey(key []byte) Option {
	return func(o *serverOptions) { o.masterKey = key }
}

// WithEmulator wires the web-tool emulator (plan §6.2 Firecrawl fallback)
// into the routing executor. Without it, emulated web tools degrade to the
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
	// admin-API provider write (Task 5 Gap #2) — no restart. Web-tool
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

	// Routing rules live in the store (Analysis E: rule state is
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
		// model to the row's provider when no rule matched. A read
		// failure degrades to no-row (ok=false) — the last-known-good
		// read contract; the request then gets the honest no-route
		// rejection, never a misrouted chain.
		routing.WithModelLookup(func(modelID string) (string, bool) {
			row, err := store.GetModel(context.Background(), modelID)
			if err != nil || row.ProviderID == "" {
				return "", false
			}
			return row.ProviderID, true
		}),
	)

	// Capability fallback models (2026-09-21 automatic capabilities): the
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

	// ADR-010 web-tool emulator pool: the stored firecrawl accounts are the
	// pool's base; the ENV emulator (o.emulator) rides as an additional
	// member when no DB account provided an emulator (the legacy ENV
	// configuration path stays functional). Pool order refinement via the
	// web_search/web_fetch fallback KVs happens on the admin reload.
	exec := routing.NewExecutor(routing.ExecutorConfig{
		Registry:            reg,
		Health:              health,
		Emulator:            buildEmulatorPool(dbEmulators, o.emulator),
		CapabilityFallbacks: capabilityFallbacks,
		OnCapabilityGap:     s.learnCapabilityGap,
	})

	// Client-facing surfaces behind client-key auth (Analysis G control 1).
	authOpts := []auth.Option{}
	if cfg.Auth.Disabled {
		authOpts = append(authOpts, auth.WithAuthDisabled())
		// Loud one-time startup warning — never silently off.
		logger.Warn("PROxyLLM: client-key auth is DISABLED (PROxyLLM_AUTH_DISABLED); every client can use the proxy without a key")
	}
	authMiddleware := auth.NewMiddleware(store, authOpts...)
	handler := httpapi.New(httpapi.Params{
		Store:    store,
		Router:   router,
		Executor: exec,
	})
	// The proxy API mounts at /v1/ (every httpapi route lives there), so the
	// SPA can own the root: a browser hitting / gets the WebUI without a
	// client key — the WebUI session is the WebUI's auth, not the client
	// key's (plan §9, §5.2).
	s.mux.Handle("/v1/", authMiddleware.Wrap(handler))

	// Admin API (plan §5.2): mounted only when the master key is wired —
	// the reload hook pushes freshly stored routing rules into the LIVE
	// router (Analysis E: WebUI rule writes take effect without restart;
	// a failed store write never reloads, so the router never sees a
	// rule set that is not the persisted one).
	//
	// Without the master key there is no admin surface to mount — but the
	// path must still answer with the auth gate's uniform 401, never the
	// SPA's 404: a 404/401 distinction would reveal whether the admin API
	// is mounted at all (the no-degraded-fallback contract from
	// TestServer_NoMasterKeyNoAdminAPI).
	if o.masterKey != nil {
		admin := webui.NewAdminAPI(webui.AdminParams{
			Store: store,
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
				exec.SetEmulator(buildEmulatorPool(emus, nil))
			},
			ReloadSettings: func() {
				// Settings KV writes reach the live runtime without restart:
				// only a successfully read alias map swaps in (SetAliases,
				// same last-known-good contract as SetRules above).
				router.SetAliases(loadModelAliases(context.Background(), store, logger))
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
				// 2026-09-21 automatic capabilities: the vision_fallback KV
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
				exec.SetEmulator(buildEmulatorPool(emus, nil))
			},
			SyncNow: func(ctx context.Context) (storage.ModelSyncState, error) {
				return modelsync.New(store, reg, logger).Run(ctx)
			},
			TestModel: s.testModel(reg),
			MasterKey: o.masterKey,
		})
		s.mux.Handle("/api/admin/", admin.Handler())
	} else {
		s.mux.Handle("/api/admin/", authMiddleware.Wrap(http.NotFoundHandler()))
	}

	// The embedded SPA owns everything else (plan §9): assets serve
	// directly, deep links fall back to index.html (Phase 2 research §5
	// option 2), and /v1/ + /api/ never reach it — their mux patterns are
	// more specific, and the handler excludes them defensively anyway.
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
	if o.masterKey != nil && o.modelSync {
		ctx, cancel := context.WithCancel(context.Background()) //nolint:gosec // cancel stored on the server (syncCancel); called on shutdown
		s.syncCancel = cancel
		syncer := modelsync.New(store, reg, logger)
		go modelsync.NewScheduler(syncer, store, logger).Run(ctx)
	}

	return s, nil
}

// buildEmulatorPool assembles the executor's web-tool emulator (ADR-010):
//   - one DB account  → that account's emulator (the pool with one member
//     behaves exactly like the pre-ADR-010 single emulator);
//   - multiple accounts → the EmulatorPool (failover per tool family);
//   - no DB accounts   → the ENV-wired emulator (o.emulator; the legacy
//     PROXYLLM_FIRECRAWL_API_KEY path) or nil (the documented
//     "unavailable" degrade state).
func buildEmulatorPool(dbEmulators []routing.Emulator, envEmulator routing.Emulator) routing.Emulator {
	switch {
	case len(dbEmulators) == 1:
		return dbEmulators[0]
	case len(dbEmulators) > 1:
		return routing.NewEmulatorPool(dbEmulators, dbEmulators)
	default:
		return envEmulator
	}
}

// loadAndReplaceProviders builds the provider set from the store and swaps
// it into the live registry's dynamic slot; the constructed web-tool
// emulators (firecrawl accounts, ADR-010) are returned for the executor's
// emulator pool. On a store failure the live set is kept (last known-good)
// and the error is logged — the contract mirrors ReloadRouting.
func loadAndReplaceProviders(ctx context.Context, store *storage.Store, logger *slog.Logger, reg *provider.LiveRegistry) ([]routing.Emulator, error) {
	fresh, emus, err := loadProviders(ctx, store, logger)
	if err != nil {
		logger.Error("provider reload failed; keeping last known-good providers", "error", err)
		return nil, err
	}
	reg.ReplaceDynamics(fresh)
	return emus, nil
}

// loadModelAliases reads the SPA's `model_aliases` settings blob (JSON
// object alias→model-id) from the settings KV, then folds in every
// provider TYPE's aliases from the shared type settings (2026-09-21: type
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
// the dist/ prefix; research E-2).
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

// handleHealth serves GET /healthz (Phase 0 contract, kept verbatim).
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
