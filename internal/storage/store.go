// Package storage implements SQLite persistence via modernc.org/sqlite
// (CGO-free): connection management, migrations, and the AES-256-GCM secret
// envelope. The redaction layer (internal/observability/redact) is built
// first and gates all logging in this package.
package storage

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/proxyllm/proxyllm/internal/config"
	_ "modernc.org/sqlite" // registers the "sqlite" driver (CGO-free)
)

// Store wraps the SQLite database handle and provides typed accessors.
type Store struct {
	db       *sql.DB
	envelope atomic.Pointer[Envelope]
}

// Open creates the SQLite store: pragmas applied per connection via DSN
// (WAL journal, busy timeout, foreign keys), single connection to avoid
// SQLITE_BUSY, per modernc.org/sqlite guidance. The master key is resolved
// (ENV over keyfile, generated 0600 on first start) and the envelope is
// initialized for secret columns.
func Open(cfg config.StorageConfig) (*Store, error) {
	// Ensure the data directory exists before SQLite creates the file.
	if dir := filepath.Dir(cfg.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("storage: create data dir: %w", err)
		}
	}

	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)",
		url.PathEscape(cfg.Path))

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", cfg.Path, err)
	}

	// Single connection is the most effective mitigation against SQLITE_BUSY
	// with modernc.org/sqlite; transactions must be short.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: ping %s: %w", cfg.Path, err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Envelope returns the secret envelope, or nil if none was set. Safe for
// concurrent use with SetEnvelope (e.g. future hot key rotation).
func (s *Store) Envelope() *Envelope {
	return s.envelope.Load()
}

// SetEnvelope attaches the secret envelope. Called by main after master
// key resolution; secret-column accessors require it.
func (s *Store) SetEnvelope(e *Envelope) {
	s.envelope.Store(e)
}

// Close releases the database handle.
func (s *Store) Close() error {
	return s.db.Close()
}
