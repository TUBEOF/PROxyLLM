// Package server wires the HTTP mux: health endpoint now; client-facing API
// surfaces (Phase 1) and the embedded WebUI (Phase 2) attach here.
package server

import (
	"log/slog"
	"net/http"

	"github.com/proxyllm/proxyllm/internal/config"
	"github.com/proxyllm/proxyllm/internal/storage"
)

// Server is the root http.Handler for PROxyLLM.
type Server struct {
	cfg    *config.Config
	store  *storage.Store
	logger *slog.Logger
	mux    *http.ServeMux
}

// New assembles the server: mux, middleware chain, and routes.
func New(cfg *config.Config, store *storage.Store, logger *slog.Logger) *Server {
	s := &Server{
		cfg:    cfg,
		store:  store,
		logger: logger,
		mux:    http.NewServeMux(),
	}

	// Phase 0: health endpoint only. API surfaces and WebUI attach later.
	s.mux.HandleFunc("GET /healthz", s.handleHealth)

	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
