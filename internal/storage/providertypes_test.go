package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite" // fixture opens the raw sqlite driver for the v2 database
)

// Spec: ADR-009 — routing rules carry a per-rule strategy ("" = inherit the
// global default, "round_robin", "fill_first"); the global default and the
// per-type provider settings ride the config-KV override layer. The
// accessors must round-trip and refuse invalid strategy values at the
// persistence boundary, before the router ever sees them.

func TestRoutingRule_StrategyRoundTrip(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	in := RoutingRule{
		ID:       "rule-rr",
		MatchModel: "claude-*",
		Chain:    []string{"ollama-cloud"},
		Priority: 1,
		Enabled:  true,
		// Explicit per-rule strategy wins over the global default.
		Strategy: "round_robin",
	}
	if err := s.PutRoutingRule(ctx, in); err != nil {
		t.Fatalf("PutRoutingRule: %v", err)
	}

	rules, err := s.ListRoutingRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 {
		t.Fatalf("stored %d rules, want 1", len(rules))
	}
	if rules[0].Strategy != "round_robin" {
		t.Errorf("strategy round-trip: got %q, want round_robin", rules[0].Strategy)
	}
}

func TestRoutingRule_EmptyStrategyInherits(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	// "" is a legal save: the rule inherits the global default at resolve
	// time (ADR-009 decision 3). Rules written before the strategy column
	// existed default to exactly this state.
	in := RoutingRule{ID: "rule-inh", MatchModel: "m", Chain: []string{"codex-1"}}
	if err := s.PutRoutingRule(ctx, in); err != nil {
		t.Fatalf("PutRoutingRule: %v", err)
	}

	rules, err := s.ListRoutingRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 {
		t.Fatalf("stored %d rules, want 1", len(rules))
	}
	if rules[0].Strategy != "" {
		t.Errorf("empty strategy must round-trip as \"\", got %q", rules[0].Strategy)
	}
}

func TestRoutingRule_UpsertUpdatesStrategy(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	first := RoutingRule{ID: "rule-1", MatchModel: "m", Chain: []string{"codex-1"}, Strategy: "fill_first"}
	if err := s.PutRoutingRule(ctx, first); err != nil {
		t.Fatal(err)
	}
	// Upsert without a strategy resets to inherit — last write wins, the
	// row is one whole value, never a field-by-field merge.
	second := RoutingRule{ID: "rule-1", MatchModel: "m", Chain: []string{"codex-1"}}
	if err := s.PutRoutingRule(ctx, second); err != nil {
		t.Fatal(err)
	}

	rules, err := s.ListRoutingRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].Strategy != "" {
		t.Errorf("upsert must overwrite strategy to \"\", got %+v", rules[0])
	}
}

func TestRoutingRule_InvalidStrategyRejected(t *testing.T) {
	s := openTokenStore(t)

	// A typo'd strategy must fail the save loudly (ADR-006 typo doctrine:
	// never persist what the resolver would silently misinterpret), not
	// survive until request time.
	for _, bad := range []string{"roundrobin", "ROUND_ROBIN", "random", "inherit"} {
		err := s.PutRoutingRule(context.Background(), RoutingRule{
			ID: "rule-bad", MatchModel: "m", Chain: []string{"codex-1"}, Strategy: bad,
		})
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("strategy %q: got %v, want ErrInvalid", bad, err)
		}
	}
}

func TestRoutingRules_MigrationFromV2(t *testing.T) {
	// A v2 database (pre-strategy) must upgrade in place: opening the store
	// runs migration v3, existing rules survive with strategy "" (inherit).
	// The fixture builds a REAL v2 database by applying migrations v1+v2
	// only (the in-package migrations list is the shipped sequence; v3 is
	// the last entry) and inserting a rule directly — no synthetic DDL that
	// could drift from the shipped schema.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	raw, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", dbPath))
	if err != nil {
		t.Fatalf("fixture: open raw db: %v", err)
	}
	if err := raw.Ping(); err != nil {
		t.Fatalf("fixture: ping: %v", err)
	}
	for _, m := range migrations[:len(migrations)-1] { // v1 + v2, NOT v3
		if _, err := raw.Exec(m); err != nil {
			t.Fatalf("fixture: apply migration: %v", err)
		}
	}
	// migrate() creates schema_migrations itself before walking the list —
	// the fixture must mirror that exact bootstrap.
	if _, err := raw.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatalf("fixture: create schema_migrations: %v", err)
	}
	for v := 1; v <= len(migrations)-1; v++ {
		if _, err := raw.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, 0)`, v); err != nil {
			t.Fatalf("fixture: record version %d: %v", v, err)
		}
	}
	if _, err := raw.Exec(`INSERT INTO routing_rules (id, match_model, match_capabilities, chain, priority, enabled)
		VALUES ('rule-pre', 'claude-*', '{}', '["codex-1","ollama-1"]', 0, 1)`); err != nil {
		t.Fatalf("fixture: seed pre-v3 rule: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("fixture: close raw db: %v", err)
	}

	// Reopen through the store: migrate() must apply v3 to the populated
	// table without touching existing rows.
	s, err := Open(testCfg(dbPath))
	if err != nil {
		t.Fatalf("Open on v2 database: %v (migration v3 must apply cleanly)", err)
	}
	defer func() { _ = s.Close() }()

	rules, err := s.ListRoutingRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 {
		t.Fatalf("migrated store has %d rules, want 1", len(rules))
	}
	if rules[0].ID != "rule-pre" {
		t.Errorf("migrated rule ID = %q, want rule-pre", rules[0].ID)
	}
	if rules[0].Strategy != "" {
		t.Errorf("pre-v3 rule strategy = %q, want \"\" (inherit)", rules[0].Strategy)
	}
	if len(rules[0].Chain) != 2 || rules[0].Chain[0] != "codex-1" {
		t.Errorf("chain did not survive migration: %v", rules[0].Chain)
	}
}

// ---------------------------------------------------------------------------
// Routing strategy KV (config override layer)
// ---------------------------------------------------------------------------

func TestRoutingStrategyKV_DefaultRoundRobin(t *testing.T) {
	s := openTokenStore(t)

	// ADR-009 decision 3: the global default is round_robin, and a
	// missing/invalid override must fall back to it — never fail closed,
	// never silently pick fill_first (the pre-ADR-009 behavior).
	got, err := s.GetRoutingStrategy(context.Background())
	if err != nil {
		t.Fatalf("GetRoutingStrategy: %v", err)
	}
	if got != "round_robin" {
		t.Errorf("default strategy = %q, want round_robin", got)
	}
}

func TestRoutingStrategyKV_RoundTrip(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	if err := s.SetRoutingStrategy(ctx, "fill_first"); err != nil {
		t.Fatalf("SetRoutingStrategy: %v", err)
	}
	got, err := s.GetRoutingStrategy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != "fill_first" {
		t.Errorf("round-trip: got %q, want fill_first", got)
	}

	// The raw KV row is visible to the settings page (the generic KV
	// surface lists every override) — same key, same value.
	v, ok, err := s.GetConfigValue(ctx, "routing_strategy")
	if err != nil || !ok || v != "fill_first" {
		t.Errorf("routing_strategy KV row = (%q, %t, %v), want (fill_first, true, nil)", v, ok, err)
	}
}

func TestRoutingStrategyKV_InvalidFallsBackToDefault(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	// A corrupted or hand-written override must never de-route the proxy:
	// the read falls back to the default (the model_aliases precedent —
	// invalid blobs skip, they do not crash).
	if err := s.SetConfigValue(ctx, "routing_strategy", "chaos"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRoutingStrategy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != "round_robin" {
		t.Errorf("invalid override: strategy = %q, want round_robin default", got)
	}
}

// ---------------------------------------------------------------------------
// Provider type settings KV
// ---------------------------------------------------------------------------

func TestProviderTypeSettings_RoundTrip(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	// Per-type shared settings (ADR-009 decision 1): one JSON object
	// keyed by type. The read returns an empty map when unset — the
	// normal state, indistinguishable from "no shared settings".
	got, err := s.GetProviderTypeSettings(ctx)
	if err != nil {
		t.Fatalf("GetProviderTypeSettings (unset): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("unset type settings: got %d entries, want 0", len(got))
	}

	in := map[string]json.RawMessage{
		"codex-oauth":  json.RawMessage(`{"issuer":"https://auth.openai.com"}`),
		"ollama-cloud": json.RawMessage(`{"base_url":"https://ollama.com"}`),
	}
	if err := s.PutProviderTypeSettings(ctx, in); err != nil {
		t.Fatalf("PutProviderTypeSettings: %v", err)
	}

	out, err := s.GetProviderTypeSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("round-trip: %d entries, want 2", len(out))
	}
	if string(out["codex-oauth"]) != string(in["codex-oauth"]) {
		t.Errorf("codex-oauth settings: got %s want %s", out["codex-oauth"], in["codex-oauth"])
	}
	if string(out["ollama-cloud"]) != string(in["ollama-cloud"]) {
		t.Errorf("ollama-cloud settings: got %s want %s", out["ollama-cloud"], in["ollama-cloud"])
	}
}

func TestProviderTypeSettings_InvalidBlobFallsBackToEmpty(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	// A corrupted blob must not crash reads (last-known-good contract: the
	// reader degrades to empty, the write path is where validation lives).
	if err := s.SetConfigValue(ctx, "provider_type_settings", "not json"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetProviderTypeSettings(ctx)
	if err != nil {
		t.Errorf("corrupted blob must read as empty, got error %v", err)
	}
	if len(got) != 0 {
		t.Errorf("corrupted blob must read as empty, got %d entries", len(got))
	}
}

func TestProviderTypeSettings_RejectsInvalidJSONValues(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	// The write boundary refuses a map whose values are not valid JSON —
	// a half-typed settings blob must not land in the KV.
	bad := map[string]json.RawMessage{"codex-oauth": json.RawMessage(`{"issuer":`)}
	if err := s.PutProviderTypeSettings(ctx, bad); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid settings value: got %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// Providers by type
// ---------------------------------------------------------------------------

func TestListProvidersByProviderType(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	seed := []Provider{
		{ID: "acc-c", Type: "ollama-cloud", Name: "Third"},
		{ID: "acc-a", Type: "ollama-cloud", Name: "First"},
		{ID: "acc-b", Type: "codex-oauth", Name: "Second"},
	}
	for _, p := range seed {
		if err := s.PutProvider(ctx, p); err != nil {
			t.Fatalf("PutProvider %s: %v", p.ID, err)
		}
	}

	accounts, err := s.ListProvidersByProviderType(ctx, "ollama-cloud")
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 {
		t.Fatalf("ollama-cloud accounts = %d, want 2", len(accounts))
	}
	// Deterministic order: id-ascending, so the accounts table (and any
	// expansion consuming the store order) is stable between requests.
	if accounts[0].ID != "acc-a" || accounts[1].ID != "acc-c" {
		t.Errorf("accounts not id-ascending: %v", []string{accounts[0].ID, accounts[1].ID})
	}

	// A type with no rows is an empty list, never an error.
	none, err := s.ListProvidersByProviderType(ctx, "custom-openai")
	if err != nil || len(none) != 0 {
		t.Errorf("empty type: got (%v, %v), want ([], nil)", none, err)
	}
}