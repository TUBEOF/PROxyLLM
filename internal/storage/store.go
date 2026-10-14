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
	"strings"
	"sync/atomic"

	"github.com/tubeof/proxyllm/internal/config"
	_ "modernc.org/sqlite" // registers the "sqlite" driver (CGO-free)
)

// maxOpenConns is the connection-pool size (ADR-016): enough that reads stop
// queueing behind each other, small enough that nothing queues behind the one
// writer SQLite admits at a time. The concurrency test asserts the behaviour
// this promises, not just the number.
const maxOpenConns = 4

// Store wraps the SQLite database handle and provides typed accessors.
type Store struct {
	db       *sql.DB
	envelope atomic.Pointer[Envelope]

	// keyHasher hashes client keys (see keyhash.go). Nil until main derives it
	// from the master key — verification then falls back to the legacy Argon2id
	// scheme, which is also what a store with no master key can offer.
	keyHasher atomic.Pointer[KeyHasher]
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

	// A small pool, not a single connection (ADR-016). One connection
	// serializes EVERY query in the process — reads included — which is fine at
	// low traffic and a throughput ceiling at high traffic: the request path runs
	// six to ten of them per request, none of which needs to wait for another
	// request's read.
	//
	// What made one connection attractive is SQLITE_BUSY, and that is handled
	// where it belongs: WAL lets readers and the single writer proceed at the
	// same time, and busy_timeout(5000) absorbs the brief write lock instead of
	// failing. Four connections is deliberately modest — past the writer there is
	// nothing to gain, since SQLite admits one writer at a time and the
	// transactions here are short.
	db.SetMaxOpenConns(maxOpenConns)

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

// SetKeyHasher attaches the client-key hasher, derived from the SAME master key
// as the envelope. main sets both at startup, adjacent — see keyhash.go for why
// client keys use a keyed MAC rather than Argon2id, and for what that means for
// master-key rotation.
func (s *Store) SetKeyHasher(h *KeyHasher) {
	s.keyHasher.Store(h)
}

// HashClientKey hashes a client key for storage: the keyed MAC when the store
// has a master-key-derived hasher, otherwise the legacy Argon2id scheme (a store
// with no master key — tests, and any embedding that never configured one).
func (s *Store) HashClientKey(secret string) (string, error) {
	if h := s.keyHasher.Load(); h != nil {
		return h.Hash(secret)
	}
	return NewArgon2Hasher().Hash(secret)
}

// VerifyClientKeyHash reports whether secret produced stored, and whether the
// stored hash is in the LEGACY scheme — the caller then rewrites that row (see
// auth.verifyKey), so an existing installation migrates itself as its keys are
// used instead of needing every key reissued.
//
// Dispatch is by the hash's own prefix, so a table holding both schemes verifies
// both.
func (s *Store) VerifyClientKeyHash(stored, secret string) (ok bool, legacy bool) {
	if strings.HasPrefix(stored, argon2HashPrefix) {
		return NewArgon2Hasher().Verify(stored, secret), true
	}
	h := s.keyHasher.Load()
	if h == nil {
		// A keyed-MAC row with no MAC key cannot be verified. Fail closed.
		return false, false
	}
	return h.Verify(stored, secret), false
}

// Close releases the database handle.
func (s *Store) Close() error {
	return s.db.Close()
}
