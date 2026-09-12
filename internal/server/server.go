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
}

// Option configures New.
type Option func(*serverOptions)

// WithProvider registers one provider with the server's registry at
// assembly time (cmd/proxyllm registers the config-defined ones; tests
// register mocks).
func WithProvider(p provider.Provider) Option {
	return func(o *serverOptions) { o.providers = append(o.providers, p) }
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
