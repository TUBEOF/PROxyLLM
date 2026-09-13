// Command fixturesrv serves a fully-seeded PROxyLLM instance for the
// Playwright visual regression suite (Phase 2 research §7 V-1..V-7).
//
// It is a REAL assembly — no network mocking: the embedded SPA, the real
// admin API, a real SQLite store seeded through the public storage APIs,
// and the real session/CSRF auth. The tests exercise the same code path a
// browser operator sees.
//
// The server is started by playwright.config.ts (webServer) and is NOT
// part of `go build ./cmd/...` deployments.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/proxyllm/proxyllm/internal/config"
	"github.com/proxyllm/proxyllm/internal/server"
	"github.com/proxyllm/proxyllm/internal/storage"
)

// seedAdmin credentials — fixed values so the Playwright login helper can
// type them; the store is disposable, this binary never runs outside CI
// and the local dev loop.
const (
	seedAdminUser = "admin"
	seedAdminPass = "proxyllm-e2e"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:4173", "listen address")
	dbPath := flag.String("db", "", "SQLite path (default: temp dir, fresh seed per run)")
	seedData := flag.Bool("seed", true, "seed representative data (false = empty-state store, only the admin account)")
	flag.Parse()

	if err := run(*addr, *dbPath, *seedData); err != nil {
		fmt.Fprintln(os.Stderr, "fixturesrv:", err)
		os.Exit(1)
	}
}

func run(addr, dbPath string, seedData bool) error {
	if dbPath == "" {
		tmp, err := os.MkdirTemp("", "proxyllm-fixturesrv-*")
		if err != nil {
			return err
		}
		dbPath = tmp + "/fixture.db"
	}

	cfg := config.Default()
	cfg.Server.Addr = addr

	// Fixed master key: the fixture store is disposable and local-only, so
	// a deterministic 32-byte key keeps runs reproducible (the same seed
	// encrypts to comparable rows) without weakening anything real.
	masterKey := make([]byte, 32)
	for i := range masterKey {
		masterKey[i] = byte(i + 1)
	}

	store, err := storage.Open(config.StorageConfig{Path: dbPath})
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	envelope, err := storage.NewEnvelope(masterKey)
	if err != nil {
		return err
	}
	store.SetEnvelope(envelope)

	if err := seed(context.Background(), store, seedData); err != nil {
		return err
	}

	srv, err := server.New(cfg, store, slog.Default(), server.WithMasterKey(masterKey))
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}
	fmt.Fprintf(os.Stderr, "fixturesrv: listening on http://%s (admin %q / %q)\n",
		addr, seedAdminUser, seedAdminPass)
	return httpSrv.ListenAndServe()
}

// seed fills the store: the admin account always (login must work), and —
// with seedData — a deterministic, representative dataset: providers,
// models with compat policies, routing rules, client keys, usage rows,
// request logs with route traces, and settings. Seeded runs render real
// data on every §9 route; the empty-state spec runs against seed=false.
func seed(ctx context.Context, store *storage.Store, seedData bool) error {
	// Admin: same hasher the bootstrap endpoint uses, so login exercises
	// the real Argon2id verify path.
	hash, err := storage.NewArgon2Hasher().Hash(seedAdminPass)
	if err != nil {
		return fmt.Errorf("seed admin: %w", err)
	}
	if err := store.CreateAdmin(ctx, "adm-fixture", seedAdminUser, hash); err != nil {
		return fmt.Errorf("seed admin: %w", err)
	}

	if !seedData {
		return nil
	}

	// Providers (storage rows; the visual suite only drives the admin API,
	// so no live provider registration is needed).
	for _, p := range []storage.Provider{
		{ID: "prv-codex", Type: "codex_oauth", Name: "ChatGPT / Codex"},
		{ID: "prv-ollama", Type: "ollama_cloud", Name: "Ollama Cloud"},
		{ID: "prv-openai", Type: "custom_openai", Name: "Custom OpenAI-compatible"},
	} {
		if err := store.PutProvider(ctx, p); err != nil {
			return fmt.Errorf("seed provider %s: %w", p.ID, err)
		}
	}

	// Models with capability flags and per-model compat policies (§6.4).
	models := []storage.Model{
		{
			ID:         "mdl-gpt5",
			ProviderID: "prv-codex",
			Name:       "gpt-5",
			Capabilities: json.RawMessage(`{"streaming":true,"tools":true,"vision":true,` +
				`"web_search":true,"thinking":true}`),
			Compat:   json.RawMessage(`{"thinking_handling":"strip","max_tokens_clamp":32000}`),
			Priority: 100,
		},
		{
			ID:         "mdl-sonnet",
			ProviderID: "prv-openai",
			Name:       "claude-sonnet-5",
			Capabilities: json.RawMessage(`{"streaming":true,"tools":true,"vision":true,` +
				`"thinking":true}`),
			Compat:   json.RawMessage(`{"thinking_handling":"forward","cache_control":"forward"}`),
			Priority: 90,
		},
		{
			ID:           "mdl-llama",
			ProviderID:   "prv-ollama",
			Name:         "llama3.3-70b",
			Capabilities: json.RawMessage(`{"streaming":true,"tools":true}`),
			Compat:       json.RawMessage(`{"thinking_handling":"map-to-reasoning-effort"}`),
			Priority:     50,
		},
	}
	for _, m := range models {
		if err := store.PutModel(ctx, m); err != nil {
			return fmt.Errorf("seed model %s: %w", m.ID, err)
		}
	}

	// Routing rules (Analysis E): a simple match + a fallback chain.
	rules := []storage.RoutingRule{
		{
			ID:         "rule-claude",
			MatchModel: "claude-sonnet-5",
			Chain:      []string{"prv-openai/claude-sonnet-5", "prv-ollama/llama3.3-70b"},
			Priority:   100,
			Enabled:    true,
		},
		{
			ID:         "rule-default",
			MatchModel: "*",
			Chain:      []string{"prv-codex/gpt-5"},
			Priority:   10,
			Enabled:    true,
		},
	}
	for _, r := range rules {
		if err := store.PutRoutingRule(ctx, r); err != nil {
			return fmt.Errorf("seed rule %s: %w", r.ID, err)
		}
	}

	// Client keys: hash-only storage via the auth package's canonical path
	// would need its CreateClientKey; storage-level rows are equivalent for
	// display (the secret is never stored anyway).
	if err := store.CreateClientKey(ctx, "ckey-claude-code", "Claude Code", "$argon2id$fixture", "pk_1a2b", nil); err != nil {
		return fmt.Errorf("seed client key: %w", err)
	}
	if err := store.CreateClientKey(ctx, "ckey-ci", "CI runner", "$argon2id$fixture", "pk_9f8e", nil); err != nil {
		return fmt.Errorf("seed client key: %w", err)
	}

	// Usage: 14 days of aggregation rows so the Overview chart renders a
	// stable time axis. Anchored to the suite's frozen clock (FROZEN_DATE in
	// webui/e2e/helpers.ts): the SPA renders usage through a 30-day window
	// ending at the page's mocked Date, so a wall-clock "today" would drift
	// out of that window as real time moves past the frozen instant.
	now := time.Date(2026, time.September, 13, 12, 0, 0, 0, time.UTC)
	for i := 13; i >= 0; i-- {
		day := now.AddDate(0, 0, -i).Format("2006-01-02")
		for _, u := range []storage.UsageDaily{
			{Day: day, ClientKeyID: "ckey-claude-code", ProviderID: "prv-openai", Model: "claude-sonnet-5",
				InputTokens: int64(12000 + i*1000), OutputTokens: int64(3000 + i*200), CacheReadTokens: int64(8000),
				Requests: int64(40 + i*3), Failures: int64(i % 3)},
			{Day: day, ClientKeyID: "ckey-ci", ProviderID: "prv-codex", Model: "gpt-5",
				InputTokens: int64(2000 + i*150), OutputTokens: int64(600 + i*40), Requests: int64(8 + i)},
		} {
			if err := store.IncrUsageDaily(ctx, u); err != nil {
				return fmt.Errorf("seed usage %s: %w", day, err)
			}
		}
	}

	// Request logs with route traces — deterministic content, relative
	// timestamps, one failure and one failover so the log viewer's
	// highlighted degradations render.
	base := now.Add(-2 * time.Hour).Unix()
	logs := []storage.RequestLog{
		{
			Timestamp: base, ClientKeyID: "ckey-claude-code", ProviderID: "prv-openai",
			Model: "claude-sonnet-5", Status: 200, LatencyMS: 842,
			Usage:      json.RawMessage(`{"input_tokens":1204,"output_tokens":318,"cache_read_input_tokens":512}`),
			RouteTrace: json.RawMessage(`{"rule":"rule-claude","chain":["prv-openai/claude-sonnet-5"],"transitions":[]}`),
		},
		{
			Timestamp: base - 300, ClientKeyID: "ckey-claude-code", ProviderID: "prv-ollama",
			Model: "llama3.3-70b", Status: 200, LatencyMS: 1930,
			Usage: json.RawMessage(`{"input_tokens":1204,"output_tokens":502}`),
			RouteTrace: json.RawMessage(`{"rule":"rule-claude","chain":["prv-openai/claude-sonnet-5","prv-ollama/llama3.3-70b"],` +
				`"transitions":[{"kind":"failover","from":"prv-openai/claude-sonnet-5","to":"prv-ollama/llama3.3-70b","reason":"upstream 503"}],` +
				`"compat":[{"policy":"thinking_handling","applied":"map-to-reasoning-effort"}]}`),
		},
		{
			Timestamp: base - 900, ClientKeyID: "ckey-ci", ProviderID: "prv-codex",
			Model: "gpt-5", Status: 429, LatencyMS: 210,
			Usage:         json.RawMessage(`{}`),
			RouteTrace:    json.RawMessage(`{"rule":"rule-default","chain":["prv-codex/gpt-5"],"transitions":[]}`),
			ErrorRedacted: "upstream rate limit exceeded",
		},
	}
	for _, l := range logs {
		if _, err := store.InsertRequestLog(ctx, l); err != nil {
			return fmt.Errorf("seed request log: %w", err)
		}
	}

	// Settings: a few KV overrides so the settings page lists real entries.
	for k, v := range map[string]string{
		"anthropic_surface_non_claude":  "allow",
		"log_retention_days":            "30",
		"request_rate_limit_per_minute": "600",
	} {
		if err := store.SetConfigValue(ctx, k, v); err != nil {
			return fmt.Errorf("seed setting %s: %w", k, err)
		}
	}

	return nil
}
