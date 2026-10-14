# ADR-003: Persistence — SQLite via modernc.org/sqlite

- **Status**: Accepted
- **Date**: 2026-09-10

## Context

PROxyLLM must persist: layered config, provider credentials (OAuth tokens,
API keys), client API keys (hashed), request logs, usage stats, WebUI admin
accounts, routing rules. Write pattern: modest OLTP plus bursty
stream-completion log inserts. Volume: MB to low GB. Deployment constraint:
**one service, one data directory** — backup must be "copy the file".

## Criteria

Transactions for concurrent WebUI writes; query/filter/aggregate for logs
and stats; backup simplicity; single-service constraint; at-rest encryption
at the application layer; retention management.

## Options considered

| Criterion | SQLite (modernc) | Config files (YAML) | Postgres | Hybrid files + JSONL |
|---|---|---|---|---|
| Transactions / concurrent writes | 5 | 1 | 5 | 2 |
| Query/filter/aggregate | 5 | 1 | 5 | 2 |
| Backup simplicity | 5 | 5 | 2 | 3 |
| Single-service constraint | 5 | 5 | 1 | 5 |
| At-rest encryption (app envelope) | 5 | 2 | 3 | 2 |
| Retention management | 5 | 1 | 5 | 2 |
| **Total** | **30** | **15** | **18** | **16** |

## Decision

**SQLite via `modernc.org/sqlite`** (pure Go, CGO-free — preserves the static
binary), WAL mode, one file in the bind-mounted data dir.

Verified against github.com/modernc.org/sqlite: DSN pragmas
`?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)`
apply to every pool connection; `SetMaxOpenConns(1)` is the most effective
mitigation against SQLITE_BUSY; transactions must stay short.

## Consequences

- Backup = copy the data dir; no external database service ever.
- Writes serialize through one connection — fine at proxy scale, but log
  ingestion must batch (usage_daily aggregation + async request_log writes).
- The pure-Go driver keeps CGO out of the build; cgo-based drivers
  (mattn/go-sqlite3) were never in contention for that reason.
- Application-layer AES-256-GCM envelope encryption for secrets — the DB file
  alone never decrypts provider credentials.