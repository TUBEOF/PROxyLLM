// Package server wires the live proxy: httpapi surfaces behind auth
// middleware, routing rules loaded from the store, providers from the
// caller, and the health endpoint (plan §4.1/§5.1, Analysis G control 1).
package server

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/proxyllm/proxyllm/internal/auth"
	"github.com/proxyllm/proxyllm/internal/config"
	"github.com/proxyllm/proxyllm/internal/provider"
	"github.com/proxyllm/proxyllm/internal/routing"
	"github.com/proxyllm/proxyllm/internal/server/httpapi"
	"github.com/proxyllm/proxyllm/internal/server/webui"
	"github.com/proxyllm/proxyllm/internal/storage"
)

// Server is the root http.Handler for PROxyLLM.
type Server struct {
	cfg    *config.Config
	store  *storage.Store
	logger *slog.Logger
	mux    *http.ServeMux
}

// serverOptions collects the New options.
type serverOptions struct {
	providers []provider.Provider
	emulator  routing.Emulator
	masterKey []byte
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
// material is wired in. Without it there is no /api/admin/ surface at all
// (404), never a weakened fallback.
func WithMasterKey(key []byte) Option {
	return func(o *serverOptions) { o.masterKey = key }
}

// WithEmulator wires the web-tool emulator (plan §6.2 Firecrawl fallback)
// into the routing executor. Without it, emulated web tools degrade to the
// documented "unavailable" error results — never silently absent.
func WithEmulator(emu routing.Emulator) Option {
	return func(o *serverOptions) { o.emulator = emu }
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

	// Provider registry: every WithProvider-registered provider joins once.
	reg := provider.NewRegistry()
	for _, p := range o.providers {
		reg.Register(p)
	}

	// Routing rules live in the store (Analysis E: rule state is
	// WebUI-editable, not in-memory registration).
	rules, err := loadRoutingRules(context.Background(), store)
	if err != nil {
		return nil, err
	}

	health := routing.NewHealthTracker()
	router := routing.NewRouter(routing.Config{Rules: rules},
		routing.WithRegistry(reg),
		routing.WithHealth(health),
	)
	exec := routing.NewExecutor(routing.ExecutorConfig{
		Registry: reg,
		Health:   health,
		Emulator: o.emulator,
	})

	// Client-facing surfaces behind client-key auth (Analysis G control 1).
	authOpts := []auth.Option{}
	if cfg.Auth.Disabled {
		authOpts = append(authOpts, auth.WithAuthDisabled())
		// Loud one-time startup warning — never silently off.
		logger.Warn("PROxyLLM: client-key auth is DISABLED (PROXYLLM_AUTH_DISABLED); every client can use the proxy without a key")
	}
	handler := httpapi.New(httpapi.Params{
		Store:    store,
		Router:   router,
		Executor: exec,
	})
	s.mux.Handle("/", auth.NewMiddleware(store, authOpts...).Wrap(handler))

	// Admin API (plan §5.2): mounted only when the master key is wired —
	// the reload hook pushes freshly stored routing rules into the LIVE
	// router (Analysis E: WebUI rule writes take effect without restart;
	// a failed store write never reloads, so the router never sees a
	// rule set that is not the persisted one).
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
			MasterKey: o.masterKey,
		})
		s.mux.Handle("/api/admin/", admin.Handler())
	}

	s.mux.HandleFunc("GET /healthz", s.handleHealth)

	return s, nil
}

// loadRoutingRules converts stored rules to router rules, skipping
// disabled ones (they must not participate in resolution).
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
			MatchModel: r.MatchModel,
			Chain:      r.Chain,
			Priority:   r.Priority,
			Enabled:    true,
		})
	}
	return rules, nil
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
