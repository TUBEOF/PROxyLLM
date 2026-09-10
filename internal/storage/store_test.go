package storage

import (
	"path/filepath"
	"testing"

	"github.com/proxyllm/proxyllm/internal/config"
)

// Spec: ADR-003 — SQLite via modernc.org/sqlite, WAL journal mode, foreign
// keys ON, single connection; migrations are ordered, append-only, and
// run exactly once (tracked in schema_migrations).

func TestOpen_MigrationsApplied(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(testCfg(filepath.Join(dir, "test.db")))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("query migrations: %v", err)
	}
	if count != len(migrations) {
		t.Errorf("applied migrations: got %d want %d", count, len(migrations))
	}

	// All Phase 0 tables exist and are queryable.
	for _, table := range []string{
		"config", "providers", "provider_tokens", "models", "routing_rules",
		"client_keys", "webui_admins", "request_logs", "usage_daily",
	} {
		// table comes from the fixed list above, not user input.
		if _, err := s.db.Exec("SELECT 1 FROM " + table + " WHERE 1=0"); err != nil { //nolint:gosec // fixed test list
			t.Errorf("table %s not queryable: %v", table, err)
		}
	}
}

func TestOpen_MigrationsRunExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	cfg := testCfg(filepath.Join(dir, "test.db"))
	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen: migrations must not re-run or duplicate.
	s2, err := Open(cfg)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() { _ = s2.Close() }()

	var count int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(migrations) {
		t.Errorf("re-open: applied migrations %d, want %d (no duplicates)", count, len(migrations))
	}
}

func TestOpen_WALAndForeignKeys(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(testCfg(filepath.Join(dir, "test.db")))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	var journalMode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" {
		t.Errorf("journal mode: got %q want wal", journalMode)
	}

	var fk int
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys: got %d want 1", fk)
	}
}

func TestOpen_SingleConnection(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(testCfg(filepath.Join(dir, "test.db")))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	// Enforced via SetMaxOpenConns(1) against SQLITE_BUSY per ADR-003.
	if s.db.Stats().MaxOpenConnections != 1 {
		t.Errorf("MaxOpenConnections: got %d want 1", s.db.Stats().MaxOpenConnections)
	}
}

func TestCRUD_ProviderAndModel(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(testCfg(filepath.Join(dir, "test.db")))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, err := s.db.Exec(`INSERT INTO providers (id, type, name) VALUES ('p1', 'ollama', 'Ollama Cloud')`); err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	// Foreign key enforced: model with unknown provider must fail.
	if _, err := s.db.Exec(`INSERT INTO models (id, provider_id, name) VALUES ('m1', 'nope', 'x')`); err == nil {
		t.Error("foreign key violation must fail")
	}
	if _, err := s.db.Exec(`INSERT INTO models (id, provider_id, name) VALUES ('m1', 'p1', 'gpt-oss:120b')`); err != nil {
		t.Fatalf("insert model: %v", err)
	}

	var name string
	if err := s.db.QueryRow(`SELECT name FROM models WHERE id='m1'`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "gpt-oss:120b" {
		t.Errorf("model name: got %q", name)
	}
}

// testCfg returns a StorageConfig for a temp-dir database.
func testCfg(path string) config.StorageConfig {
	return config.StorageConfig{Path: path}
}
