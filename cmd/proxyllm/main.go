// Command proxyllm runs the PROxyLLM LLM proxy server.
//
// Phase 0 scope: parse layered configuration, initialize storage, wire the
// HTTP mux, and serve a health endpoint. Providers, translation, and the
// admin WebUI land in later phases.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/proxyllm/proxyllm/internal/config"
	"github.com/proxyllm/proxyllm/internal/observability"
	"github.com/proxyllm/proxyllm/internal/server"
	"github.com/proxyllm/proxyllm/internal/storage"
	"github.com/proxyllm/proxyllm/internal/tools/firecrawl"
)

// loadMasterKey resolves the envelope master key per the documented order:
// PROXYLLM_MASTER_KEY ENV, then keyfile.
func loadMasterKey(cfg *config.Config) ([]byte, error) {
	envVal, envSet := config.MasterKeyFromEnv()
	return storage.LoadMasterKey(envVal, envSet, cfg.Storage.MasterKeyPath)
}

// serverOptions assembles the server wiring options from config. The
// web-tool emulator (plan §6.2) activates only when a Firecrawl API key is
// configured: without one there is nothing to emulate with, and the
// emulation loop then degrades to the documented "unavailable" error
// results. Never silently off — the caller logs the state.
func serverOptions(cfg *config.Config, masterKey []byte) []server.Option {
	opts := []server.Option{
		// Mount the admin API (plan §5.2): the master key doubles as the
		// session/CSRF HMAC secret — the same bytes that sealed the
		// storage envelope, so WebUI sessions and at-rest secrets break
		// and rotate together.
		server.WithMasterKey(masterKey),
	}
	if apiKey, ok := config.FirecrawlAPIKeyFromEnv(); ok && apiKey != "" {
		opts = append(opts, server.WithEmulator(
			firecrawl.NewEmulator(cfg.Firecrawl.BaseURL, apiKey, nil)))
	}
	return opts
}

func main() {
	// `-healthcheck` is the container healthcheck entry: probe the
	// server's /healthz and exit 0/1. Kept in-binary so the distroless
	// image (no curl/wget) can healthcheck without extra tooling.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck(os.Getenv("PROXYLLM_HEALTHCHECK_ADDR")))
	}
	// Install the redacting handler on the DEFAULT logger before run() so
	// fatal errors from early startup (config load, logger init, storage
	// open, master key resolution) cannot bypass redaction — ADR-007
	// control 3: no log call site reaches stderr unredacted. Level/format
	// configuration is re-applied inside run().
	slog.SetDefault(slog.New(observability.NewRedactingHandler(
		slog.NewJSONHandler(os.Stdout, nil), observability.NewRedactor())))
	if err := run(); err != nil {
		slog.Default().Error("fatal", "error", err)
		os.Exit(1)
	}
}

// healthcheck performs an HTTP GET against /healthz. The target address
// is PROXYLLM_HEALTHCHECK_ADDR if set, else the server bind
// PROXYLLM_SERVER_ADDR, else the default :8080. All output is redacted
// like every other log call site (ADR-007 control 3).
func healthcheck(addr string) int {
	red := observability.NewRedactor()
	if addr == "" {
		addr = os.Getenv("PROXYLLM_SERVER_ADDR")
	}
	if addr == "" {
		addr = "localhost:8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "localhost" + addr
	}
	client := &http.Client{Timeout: 3 * time.Second}
	// addr is operator-controlled (ENV or localhost default), never
	// request-derived input — no SSRF taint in practice.
	resp, err := client.Get("http://" + addr + "/healthz") //nolint:gosec // operator-controlled address
	if err != nil {
		fmt.Fprintln(os.Stderr, red.Redact("healthcheck failed: "+err.Error()))
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck failed: status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

func run() error {
	// Config: defaults -> file -> ENV -> DB (highest).
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger, err := observability.NewLogger(cfg.Log)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	// No auth-disabled warning here: server.New emits the canonical loud
	// one-time startup warning when cfg.Auth.Disabled is set (ADR-007
	// control 1 — the wiring layer owns it, so there is exactly one).

	store, err := storage.Open(cfg.Storage)
	if err != nil {
		return err
	}
	defer func() {
		if err := store.Close(); err != nil {
			logger.Warn("store close", "error", err)
		}
	}()

	// Resolve the envelope master key (ENV PROXYLLM_MASTER_KEY over
	// keyfile; generated 0600 on first start) and attach the envelope to
	// the store so secret columns are encrypted at rest.
	masterKey, err := loadMasterKey(cfg)
	if err != nil {
		return err
	}
	envelope, err := storage.NewEnvelope(masterKey)
	if err != nil {
		return err
	}
	store.SetEnvelope(envelope)

	// Web-tool emulation state (plan §6.2) is loud in both directions:
	// active when a Firecrawl key is configured, degraded (documented
	// "unavailable" error results) without one.
	if _, ok := config.FirecrawlAPIKeyFromEnv(); !ok {
		logger.Warn("PROxyLLM: web-tool emulation is inactive (PROXYLLM_FIRECRAWL_API_KEY not set); emulated web_search/web_fetch requests will receive the documented \"unavailable\" error results")
	}

	srv, err := server.New(cfg, store, logger, serverOptions(cfg, masterKey)...)
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Serve until interrupted, then shut down gracefully.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", httpSrv.Addr)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}
