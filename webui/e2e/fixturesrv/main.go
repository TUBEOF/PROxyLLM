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

	"github.com/tubeof/proxyllm/internal/config"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/server"
	"github.com/tubeof/proxyllm/internal/storage"
)

// seedAdmin credentials — fixed values so the Playwright login helper can
// type them; the store is disposable, this binary never runs outside CI
// and the local dev loop.
const (
	seedAdminUser = "admin"
	seedAdminPass = "proxyllm-e2e" //nolint:gosec // fixture seed for the disposable test store, not a real credential
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

	srv, err := server.New(cfg, store, slog.Default(),
		server.WithMasterKey(masterKey),
		// The scripted test-model provider (2026-09-22): the Models page's
		// Test button hits it with a "Sag OK" complete — deterministic
		// "OK" answer, no live upstream, no wall-clock state (latency is
		// measured but not rendered in the badge).
		server.WithProvider(&fixtureTestProvider{}),
	)
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
	// so no live provider registration is needed). Type strings match the
	// provider-type catalog (providertypes.go). ADR-009: multiple accounts
	// per type — the accounts list and card counts render real state.
	for _, p := range []storage.Provider{
		{ID: "prv-codex", Type: "codex-oauth", Name: "Work account"},
		{ID: "prv-codex-2", Type: "codex-oauth", Name: "Personal account"},
		{ID: "prv-ollama", Type: "ollama-cloud", Name: "Ollama key 1"},
		{ID: "prv-ollama-2", Type: "ollama-cloud", Name: "Ollama key 2"},
		{ID: "prv-openai", Type: "custom-openai", Name: "Custom OpenAI-compatible"},
		{ID: "prv-test", Type: "custom-openai", Name: "Test target"},
	} {
		if err := store.PutProvider(ctx, p); err != nil {
			return fmt.Errorf("seed provider %s: %w", p.ID, err)
		}
	}
	// Credentials: the connected accounts carry sealed token rows (the
	// accounts table renders the badge from their presence); one codex
	// draft stays credential-less to exercise "Sign-in required".
	for _, id := range []string{"prv-codex", "prv-ollama", "prv-ollama-2"} {
		if err := store.PutProviderTokens(ctx, storage.ProviderToken{
			ProviderID: id, Kind: "access_token", Secret: []byte("fixture-token"),
		}); err != nil {
			return fmt.Errorf("seed token %s: %w", id, err)
		}
	}

	// ADR-009: shared type settings (codex issuer) + the global routing
	// strategy, both through the same KV surface the admin API writes.
	if err := store.PutProviderTypeSettings(ctx, map[string]json.RawMessage{
		"codex-oauth": json.RawMessage(`{"issuer":"https://auth.openai.com"}`),
	}); err != nil {
		return fmt.Errorf("seed type settings: %w", err)
	}
	if err := store.SetRoutingStrategy(ctx, "round_robin"); err != nil {
		return fmt.Errorf("seed routing strategy: %w", err)
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
		// The test-button target (2026-09-22): the Playwright spec clicks
		// THIS model's Test button — the scripted provider below answers
		// deterministically ("OK", fixed reply, no wall-clock data in the
		// badge — latency is not rendered).
		{
			ID:           "mdl-test",
			ProviderID:   "prv-test",
			Name:         "ok-model",
			Capabilities: json.RawMessage(`{"streaming":true,"tools":true,"vision":true}`),
			Compat:       json.RawMessage(`{"thinking_handling":"strip"}`),
			Priority:     10,
		},
	}
	for _, m := range models {
		if err := store.PutModel(ctx, m); err != nil {
			return fmt.Errorf("seed model %s: %w", m.ID, err)
		}
	}

	// Routing rules (Analysis E + ADR-009): an exact-account chain, a
	// type-entry rule (expands to all accounts of the type at request
	// time), and one with an explicit per-rule strategy override.
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
			Strategy:   "fill_first",
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
			{Day: day, ClientKeyID: "ckey-claude-code", ProviderID: "prv-ollama", Model: "glm-5.3-flash",
				InputTokens: int64(900 + i*100), OutputTokens: int64(250 + i*30), Requests: int64(6 + i%4)},
		} {
			if err := store.IncrUsageDaily(ctx, u); err != nil {
				return fmt.Errorf("seed usage %s: %w", day, err)
			}
		}
	}

	// ADR-012 Quota-page cards: one LIVE blob (the ollama settings shape,
	// frozen reset times near the clock) and one local-only account. The
	// blobs are seeded straight into the KV — the fixturesrv never scrapes
	// (the no-live-upstream-calls doctrine).
	sessionPct := 28.8
	weeklyPct := 71.2
	if err := store.PutAccountUsage(ctx, "prv-ollama", storage.AccountUsageBlob{
		FetchedAt:    now.Unix(),
		Source:       "ollama",
		Plan:         "Pro",
		SessionPct:   &sessionPct,
		SessionReset: now.Add(4 * time.Hour).Add(59 * time.Minute).Unix(),
		WeeklyPct:    &weeklyPct,
		WeeklyReset:  now.Add(5 * time.Hour).Add(11 * time.Minute).Unix(),
	}); err != nil {
		return fmt.Errorf("seed ollama usage blob: %w", err)
	}

	// Request logs — deterministic content, relative timestamps. Four rows
	// (Logs-page redesign): a conversation chain (two rows sharing
	// conv_a1b2c3d4 → one gutter line), and a 429→200 healing pair sharing
	// conv_de1a7e00 (chain + healed chip on the 200). Route traces are the
	// real chainTrace v6 shape — the drawer + derivations (REQUESTED,
	// healed) see production-shaped content. No clocks beyond the shared
	// relative-offset pattern.
	base := now.Add(-2 * time.Hour).Unix()
	logs := []storage.RequestLog{
		{
			Timestamp: base, ClientKeyID: "ckey-claude-code", ProviderID: "prv-openai",
			Model: "claude-sonnet-5", Status: 200, LatencyMS: 842,
			Usage:          json.RawMessage(`{"input_tokens":1204,"output_tokens":318,"cache_read_input_tokens":512}`),
			RouteTrace:     json.RawMessage(`{"version":6,"events":[{"type":"model_resolved","from":"claude-sonnet-5","to":"claude-sonnet-5","provider_id":"prv-openai"},{"type":"attempt","provider_id":"prv-openai","model":"claude-sonnet-5"}]}`),
			ConversationID: "conv_a1b2c3d4", ConvKey: "sess:11223344-5566-7788-99aa-bbccddeeff00",
			Protocol: "anthropic", Features: json.RawMessage(`{"streaming":true,"tools":true,"thinking":false,"web_search":false,"web_fetch":false,"vision":false}`),
		},
		{
			// Second turn of the same conversation → chain line in the gutter.
			Timestamp: base - 300, ClientKeyID: "ckey-claude-code", ProviderID: "prv-ollama",
			Model: "llama3.3-70b", Status: 200, LatencyMS: 1930,
			Usage:          json.RawMessage(`{"input_tokens":1204,"output_tokens":502}`),
			RouteTrace:     json.RawMessage(`{"version":6,"events":[{"type":"model_resolved","from":"llama3.3-70b","to":"llama3.3-70b","provider_id":"prv-ollama"},{"type":"attempt","provider_id":"prv-openai","model":"claude-sonnet-5"},{"type":"failover","from":"prv-openai/claude-sonnet-5","to":"prv-ollama/llama3.3-70b","reason":"upstream 503"},{"type":"attempt","provider_id":"prv-ollama","model":"llama3.3-70b"},{"type":"compat_policy","policy":"thinking_handling","applied":"map-to-reasoning-effort"}]}`),
			ConversationID: "conv_a1b2c3d4", ConvKey: "sess:11223344-5566-7788-99aa-bbccddeeff00",
			Protocol: "anthropic", Features: json.RawMessage(`{"streaming":true,"tools":true,"thinking":true,"web_search":false,"web_fetch":false,"vision":false}`),
		},
		{
			// The failure leg of the healing pair.
			Timestamp: base - 900, ClientKeyID: "ckey-ci", ProviderID: "prv-codex",
			Model: "gpt-5", Status: 429, LatencyMS: 210,
			Usage:          json.RawMessage(`{}`),
			RouteTrace:     json.RawMessage(`{"version":6,"events":[{"type":"model_resolved","from":"gpt-5","to":"gpt-5","provider_id":"prv-codex"},{"type":"failed","class":"rate_limit","http_status":429}]}`),
			ErrorRedacted:  "upstream rate limit exceeded",
			ConversationID: "conv_de1a7e00", ConvKey: "sys:ci-smoke",
			Protocol: "openai", Features: json.RawMessage(`{"streaming":false,"tools":false,"thinking":false,"web_search":false,"web_fetch":false,"vision":false}`),
		},
		{
			// The healed leg: same conversation, succeeded after the 429 —
			// the cross-row heal chip renders (previous failed, this 2xx).
			Timestamp: base - 895, ClientKeyID: "ckey-ci", ProviderID: "prv-ollama",
			Model: "glm-5.3-flash", Status: 200, LatencyMS: 1180,
			Usage:          json.RawMessage(`{"input_tokens":88211,"output_tokens":72,"cache_read_input_tokens":88128}`),
			RouteTrace:     json.RawMessage(`{"version":6,"events":[{"type":"model_resolved","from":"glm-5.3-flash","to":"ollama-cloud/glm-5.3-flash","provider_id":"prv-ollama"},{"type":"attempt","provider_id":"prv-codex","model":"gpt-5"},{"type":"failed","provider_id":"prv-codex","class":"capability_gap","error_code":"unsupported_image","http_status":400,"failure_position":"pre_stream","capability":"vision","latency_ms":140},{"type":"capability_fallback","capability":"vision","reason":"chain exhausted on capability gaps; trying the capability fallback models"},{"type":"failover","from":"prv-codex","to":"prv-ollama","reason":"capability fallback model"},{"type":"attempt","provider_id":"prv-ollama","model":"glm-5.3-flash"}]}`),
			ConversationID: "conv_de1a7e00", ConvKey: "sys:ci-smoke",
			Protocol: "anthropic", Features: json.RawMessage(`{"streaming":false,"tools":false,"thinking":false,"web_search":false,"web_fetch":false,"vision":false}`),
		},
	}
	for _, l := range logs {
		id, err := store.InsertRequestLog(ctx, l)
		if err != nil {
			return fmt.Errorf("seed request log: %w", err)
		}
		// Sub-operations (2026-10-07): the rows whose traces carry a failover
		// / capability-fallback / emulated-tool event get their child rows by
		// the same derivation the server uses, so the fixture exercises the
		// real parent→sub-op rendering (nested table rows + drawer list).
		if err := store.InsertChildLogs(ctx, id, fixtureChildren(l.RouteTrace, base)); err != nil {
			return fmt.Errorf("seed child logs: %w", err)
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

// fixtureChildren derives a seeded row's sub-operations with the same
// mapping the server uses (routing.ChildOps over the trace events), so the
// fixture's parent→sub-op content is production-shaped rather than a
// hand-written approximation. A trace that does not parse yields no
// children — the row simply renders without them.
func fixtureChildren(trace json.RawMessage, ts int64) []storage.ChildLog {
	var t struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(trace, &t); err != nil {
		return nil
	}
	events := make([]routing.Event, 0, len(t.Events))
	for _, raw := range t.Events {
		var ev routing.Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			continue
		}
		events = append(events, ev)
	}
	ops := routing.ChildOps(events)
	out := make([]storage.ChildLog, 0, len(ops))
	for _, op := range ops {
		out = append(out, storage.ChildLog{
			TS: ts, Kind: string(op.Kind), ProviderID: op.ProviderID,
			Model: op.Model, FromRef: op.From, ToRef: op.To, Detail: op.Detail,
		})
	}
	return out
}
