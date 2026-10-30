package storage

import "fmt"

// migrations is the ordered, append-only list of schema migrations. Each
// entry runs exactly once; the applied version is tracked in
// schema_migrations. Never edit a shipped migration — append a new one.
var migrations = []string{
	// v1: core configuration and provider registry.
	`CREATE TABLE IF NOT EXISTS config (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);`,
	`CREATE TABLE IF NOT EXISTS providers (
		id   TEXT PRIMARY KEY,
		type TEXT NOT NULL,
		name TEXT NOT NULL
	);`,
	`CREATE TABLE IF NOT EXISTS provider_tokens (
		provider_id     TEXT NOT NULL REFERENCES providers(id),
		kind            TEXT NOT NULL,
		token_encrypted BLOB NOT NULL,
		expires_at      INTEGER,
		last_refresh    INTEGER,
		PRIMARY KEY (provider_id, kind)
	);`,
	`CREATE TABLE IF NOT EXISTS models (
		id           TEXT PRIMARY KEY,
		provider_id  TEXT NOT NULL REFERENCES providers(id),
		name         TEXT NOT NULL,
		capabilities TEXT NOT NULL DEFAULT '{}',
		compat       TEXT NOT NULL DEFAULT '{}',
		priority     INTEGER NOT NULL DEFAULT 0
	);`,
	`CREATE TABLE IF NOT EXISTS routing_rules (
		id                TEXT PRIMARY KEY,
		match_model       TEXT,
		match_capabilities TEXT NOT NULL DEFAULT '{}',
		chain             TEXT NOT NULL,
		priority          INTEGER NOT NULL DEFAULT 0,
		enabled           INTEGER NOT NULL DEFAULT 1
	);`,
	`CREATE TABLE IF NOT EXISTS client_keys (
		id           TEXT PRIMARY KEY,
		name         TEXT NOT NULL,
		key_hash     TEXT NOT NULL,
		key_prefix   TEXT NOT NULL,
		scopes       TEXT NOT NULL DEFAULT '[]',
		created_at   INTEGER NOT NULL,
		last_used_at INTEGER,
		revoked      INTEGER NOT NULL DEFAULT 0
	);`,
	`CREATE TABLE IF NOT EXISTS webui_admins (
		id            TEXT PRIMARY KEY,
		username      TEXT NOT NULL UNIQUE,
		password_hash TEXT NOT NULL
	);`,
	`CREATE TABLE IF NOT EXISTS request_logs (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		ts            INTEGER NOT NULL,
		client_key_id TEXT,
		provider_id   TEXT,
		model         TEXT,
		status        INTEGER NOT NULL,
		latency_ms    INTEGER NOT NULL,
		usage         TEXT NOT NULL DEFAULT '{}',
		route_trace   TEXT NOT NULL DEFAULT '{}',
		error_redacted TEXT
	);`,
	`CREATE INDEX IF NOT EXISTS idx_request_logs_ts ON request_logs(ts);`,
	`CREATE TABLE IF NOT EXISTS usage_daily (
		day               TEXT NOT NULL,
		client_key_id     TEXT,
		provider_id       TEXT,
		model             TEXT,
		input_tokens      INTEGER NOT NULL DEFAULT 0,
		output_tokens     INTEGER NOT NULL DEFAULT 0,
		cache_read_tokens INTEGER NOT NULL DEFAULT 0,
		requests          INTEGER NOT NULL DEFAULT 0,
		failures          INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (day, client_key_id, provider_id, model)
	);`,
	// v2: WebUI support — per-type non-secret provider config,
	// hashed-at-rest WebUI sessions, and an index backing the log-viewer
	// filter queries. Append-only: v1 statements are never edited.
	`ALTER TABLE providers ADD COLUMN config TEXT NOT NULL DEFAULT '{}';`,
	`CREATE TABLE IF NOT EXISTS webui_sessions (
		id_hash            TEXT PRIMARY KEY,
		admin_id           TEXT NOT NULL REFERENCES webui_admins(id) ON DELETE CASCADE,
		created_at         INTEGER NOT NULL,
		last_seen_at       INTEGER NOT NULL,
		absolute_expires_at INTEGER NOT NULL
	);`,
	`CREATE INDEX IF NOT EXISTS idx_request_logs_model ON request_logs(model);`,
	`CREATE INDEX IF NOT EXISTS idx_request_logs_provider ON request_logs(provider_id);`,
	`CREATE INDEX IF NOT EXISTS idx_request_logs_status ON request_logs(status);`,
	// v3: ADR-009 — per-rule routing strategy. "" (the default) inherits the
	// global default from the config-KV (routing_strategy); the canonical
	// values are round_robin and fill_first. Append-only: v1/v2 statements
	// are never edited; existing rules read as "" = inherit.
	`ALTER TABLE routing_rules ADD COLUMN strategy TEXT NOT NULL DEFAULT '';`,
	// v4: ADR-010 model sync — every model row records where it came from:
	// "manual" (operator-created, sync never touches it) or "synced"
	// (imported/updated by the model-sync run, deletable when the upstream
	// drops it). Existing rows read as "manual" — pre-sync models were all
	// operator-curated, and the sync must never delete them.
	`ALTER TABLE models ADD COLUMN origin TEXT NOT NULL DEFAULT 'manual';`,
	// v5: Logs-page redesign — per-request protocol surface, feature
	// flags, and conversation identity. conversation_id is derived only for
	// rows written after this patch (no backfill — pre-patch rows read as
	// "" and render as "—" in the WebUI). conv_key is the deterministic
	// derivation input persisted so the rolling-window conversation
	// assignment (RecentConversation) is a single indexed query; it is
	// internal grouping state, never exposed on the wire. Append-only:
	// v1-v4 statements are never edited.
	`ALTER TABLE request_logs ADD COLUMN conversation_id TEXT NOT NULL DEFAULT '';`,
	`ALTER TABLE request_logs ADD COLUMN conv_key TEXT NOT NULL DEFAULT '';`,
	`ALTER TABLE request_logs ADD COLUMN protocol TEXT NOT NULL DEFAULT '';`,
	`ALTER TABLE request_logs ADD COLUMN features TEXT NOT NULL DEFAULT '{}';`,
	`CREATE INDEX IF NOT EXISTS idx_request_logs_conversation ON request_logs(conversation_id);`,
	`CREATE INDEX IF NOT EXISTS idx_request_logs_convkey ON request_logs(client_key_id, conv_key, id);`,
	// The single-column indexes above cover the equality filter but not the
	// requested order, so a filtered log page sorts its whole match set before
	// taking one page. Measured against 200k rows: the unfiltered first page —
	// the one the open Logs page repeats — walks the ts index and stops at the
	// limit in 0.02 ms, while a filtered page costs 7-16 ms. Not worth another
	// index: every one of them is write amplification on the insert path of
	// every proxied request, and these two are interactive rather than repeated.
	// The fix, if a filtered page ever becomes a hot read, is a composite
	// (filter column, ts) index — and this measurement is what should justify
	// it. See docs/deployment.md for the query plans.
	// Logs-page sub-operations. A request's separable upstream
	// operations — a chain failover, a capability (vision) fallback model
	// walk, an emulated web-tool call, a same-provider retry — get their own
	// row here instead of living only inside the parent's route_trace. A
	// separate table (not a parent_id column on request_logs) keeps every
	// request_logs row a request, so the log viewer's pagination and its
	// total count stay unchanged. parent_id references request_logs(id) by
	// convention, not by FK, so a request retention cleanup cannot cascade
	// unexpectedly. Append-only: all earlier statements are never edited.
	`CREATE TABLE IF NOT EXISTS request_child_logs (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		parent_id   INTEGER NOT NULL,
		ts          INTEGER NOT NULL,
		kind        TEXT NOT NULL,
		provider_id TEXT,
		model       TEXT,
		from_ref    TEXT,
		to_ref      TEXT,
		detail      TEXT
	);`,
	`CREATE INDEX IF NOT EXISTS idx_request_child_logs_parent ON request_child_logs(parent_id);`,

	// v7: request payloads (Logs page). The conversation a request
	// carried and the answer it produced, so the log can be read rather than
	// only counted. A table of its own, not columns on request_logs: the list
	// query runs on every page poll and must not drag megabytes of prompt text
	// out of disk with it. Redacted and size-capped before they land here (see
	// PutRequestPayload), and pruned on a retention window — nothing bounded
	// them before, which is why they did not exist.
	`CREATE TABLE IF NOT EXISTS request_payloads (
		owner_kind  TEXT NOT NULL,
		owner_id    INTEGER NOT NULL,
		ts          INTEGER NOT NULL,
		client_request  TEXT,
		client_response TEXT,
		payload_bytes   INTEGER NOT NULL DEFAULT 0,
		truncated       INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (owner_kind, owner_id)
	);`,
	`CREATE INDEX IF NOT EXISTS idx_request_payloads_ts ON request_payloads(ts);`,

	// v8: what a sub-operation cost. A child row could name where a step ran
	// but not what it spent: the describer call's tokens and the emulated web
	// call's duration existed only inside the parent request's totals, and the
	// log could not say which of several steps the time went to. usage mirrors
	// request_logs.usage (the same JSON object; '{}' when the step bills
	// nothing) so the WebUI renders the same input/output/cache triple from it.
	`ALTER TABLE request_child_logs ADD COLUMN latency_ms INTEGER NOT NULL DEFAULT 0;`,
	`ALTER TABLE request_child_logs ADD COLUMN usage TEXT NOT NULL DEFAULT '{}';`,

	// v9: accounts can be switched off. A disabled account is not
	// constructed into the live registry at all — out of routing, out of the
	// type's chain expansion, out of the health picture, out of the model
	// sync and out of the web-tool emulator pool, with its tokens and model
	// rows left in place so switching it back on restores it whole. Existing
	// rows read as enabled: an installation that never touched the switch
	// behaves exactly as before. Append-only: earlier statements are never
	// edited.
	`ALTER TABLE providers ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1;`,

	// Deleted client keys are actually deleted. The Keys page used to
	// keep revoked rows as an audit trail; the operator's rule is that a
	// deleted key leaves the database. Purge the soft-deleted rows once;
	// DeleteClientKey is a hard delete from here on. The `revoked` column
	// stays (default 0) — no destructive schema change, and existing rows
	// read as active. Historical request_logs.client_key_id strings are
	// untouched (no foreign key); their name projection falls back to the id.
	`DELETE FROM client_keys WHERE revoked = 1;`,
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	);`); err != nil {
		return fmt.Errorf("storage: create schema_migrations: %w", err)
	}

	var current int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("storage: read schema version: %w", err)
	}

	for i, m := range migrations {
		v := i + 1
		if v <= current {
			continue
		}
		// The DDL and its version row commit together: a crash between them
		// must not re-run a non-idempotent ALTER on the next start.
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("storage: begin migration %d: %w", v, err)
		}
		if _, err := tx.Exec(m); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("storage: apply migration %d: %w", v, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, strftime('%s','now'))`, v); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("storage: record migration %d: %w", v, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("storage: commit migration %d: %w", v, err)
		}
	}
	return nil
}
