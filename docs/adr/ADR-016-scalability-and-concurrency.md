# ADR-016: Scalability and concurrency for hundreds of clients

- **Status**: Accepted
- **Date**: 2026-10-10
- **Related**: ADR-003 (SQLite via modernc), ADR-007 (storage/auth/security),
  ADR-015 (web-tool resilience)

## Context

The proxy is expected to serve several hundred concurrent users. Reading the
request path with that number in mind turned up work that is negligible at one
client and a ceiling at hundreds — none of it a correctness bug, all of it paid
per request:

1. **A KDF on every authenticated request.** Client keys were verified with
   Argon2id (19 MiB, t=2, plus a per-request `last_used_at` write). Measured on
   this machine: an authenticated request cost **86.7 ms** p99 against **15.7 ms**
   for the same path with the key check removed — the difference is the KDF and
   the extra DB round trips, on every request.
2. **One SQLite connection for the whole process.** Six to ten queries run per
   request, all serialized on that single connection, reads included.
3. **An untuned HTTP transport.** Go's `MaxIdleConnsPerHost` default is 2, so
   beyond two in-flight requests to one upstream nothing is pooled: a burst of 8
   concurrent requests, twice, opened **11** connections instead of 8.
4. **Locks that serialize unrelated work.** The live-log hub took one global
   mutex once per subscriber per event; the provider registry took a write lock
   for reads; the router took its rule mutex to increment a round-robin counter;
   the health tracker had one mutex for every operation.
5. **Two unbounded waits in the emulation path** (a tool call and the round-trip
   loop), addressed in ADR-015.

## Criteria

- Remove per-request work that scales with clients, not with requests.
- Keep the security properties: a stolen database must still not yield client
  keys, and revocation must stay immediate.
- Prefer measured claims over asserted ones, and keep the measurements
  machine-independent where the host is unreliable.

## Options considered

| Option | Strengths | Weaknesses |
|---|---|---|
| Keep Argon2id, add a short-lived verify cache | No format change | A revoked key keeps working until the TTL; new state to reason about |
| Keyed MAC (HMAC-SHA256) under a master-key-derived secret | Removes the KDF from the request path entirely; standard for API keys | Ties client-key hashes to the master key (rotation must reissue keys) |
| Raise the SQLite connection limit only | Trivial | Readers still queue behind the writer's transactions |
| Split readers and a single writer onto two handles | Deterministic writes | More state; unnecessary while WAL + busy_timeout hold |
| Move request logging to an async writer | Removes the log write from the handler | The live SSE row and `reqlog.finish` depend on the synchronous rowid — a real refactor for a cost not yet measured |

## Decision

**Take the per-request costs out: client keys verify with a keyed MAC, the store
gets a small connection pool, the transport is tuned, and the locks that
serialize unrelated work are split. Defer the invasive changes until a
measurement asks for them.**

- **Client keys**: `$hmacsha256$v=1$<salt>$<mac>`, MAC key
  `HMAC-SHA256(masterKey, "proxyllm.client-key-hash/v1")` derived once at
  startup. Verification is a constant-time compare; a 256-bit CSPRNG secret has
  nothing for a KDF to protect. Legacy Argon2id rows keep verifying and are
  rewritten on their first successful use, so no issued key stops working.
  Admin passwords stay on Argon2id: they are human-chosen, low-entropy secrets.
- **Store**: `MaxOpenConns` 1 → 4. WAL lets readers and the single writer
  proceed concurrently and `busy_timeout(5000)` absorbs the write lock; the pool
  exists so that reads stop queueing behind each other.
- **Transport**: `MaxIdleConnsPerHost` 32, `MaxIdleConns` 200, an explicit dial
  timeout, `ResponseHeaderTimeout` 30 s, `IdleConnTimeout` 90 s. Accounts of one
  emulator pool share a single client (ADR-015).
- **Locks**: the log hub uses two locks and a per-subscriber lock with a closed
  flag, so the fan-out never holds a global lock; the provider registry's reads
  take a reader lock; the round-robin counter is atomic per rule; the health
  tracker's reads take a reader lock.

## Consequences

Easier: authentication no longer dominates a request (measured: the auth-on
path's p99 fell from 86.7 ms to 16.6 ms here, against 15.7 ms with the key check
removed — the gap is now within noise, which is what
`internal/e2e/scalability_test.go` asserts); bursts reuse connections; a
request's registration no longer queues behind the log broadcast.

Harder / accepted:

- **Master-key rotation now also invalidates client keys.** They are
  display-once, so the secrets cannot be recovered to be re-hashed. Rotation
  already requires re-entering provider credentials, so this widens an existing
  operation rather than adding a new one; the versioned hash prefix leaves room
  for a multi-key ring if that becomes worth building. A store with no master
  key keeps the legacy scheme (tests, and any embedding that never configured
  one) — verification is by the hash's own prefix, so a mixed table is valid.
- **A read/write split is deferred** until `SQLITE_BUSY` is actually observed;
  the concurrency test asserts correctness (every request served and logged)
  rather than a timing this host cannot measure honestly.
- **The async log writer is deferred**: it would decouple the live SSE row from
  the synchronous rowid, and the current measurement does not justify that.
- **Health sharding** (one mutex per provider shard) is deferred for the same
  reason.
- **The absolute latency budget (10 ms p99) is unchanged and still needs a
  quiet-runner recalibration**: this container measures 16.6 ms for code the
  release runner historically measured at 2.6-5.9 ms. The measurement was
  switched to the PRODUCTION path (key check included), which is what makes the
  number meaningful now that the KDF is gone.
