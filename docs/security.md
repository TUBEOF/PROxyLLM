# Security Guide

Threat model and controls summary (full concept: Analysis G in the plan,
[ADR-007](adr/ADR-007-security-concept.md)). Scope: a LAN-deployed
single-tenant proxy operated by one operator. **Out of scope for v1**:
internet-scale multi-tenant exposure — if you expose the proxy to the
public internet, put an authenticating reverse proxy with TLS and rate
limiting in front and harden beyond this document (documented
at-operator's-risk guidance).

## Authentication (default ON)

- **Client API keys** (`pk_<prefix>_<secret>`, 256-bit) gate both model
  surfaces. The secret is shown exactly once at creation, stored as an
  Argon2id hash only (plus display prefix), revocable in the WebUI.
- **WebUI admin login** (Argon2id) gates the admin API; sessions are
  HMAC-signed cookies with CSRF protection.
- **Opt-out** only via explicit ENV `PROXYLLM_AUTH_DISABLED=1` — never via
  config file or WebUI. A loud one-time startup warning is emitted
  (control 1).
- No WebUI admin mounted without a master key: `/api/admin/` answers with
  the uniform 401 (no 404/401 distinction that would reveal the mounting).

## Secrets at rest

- Provider credentials (OAuth tokens, API keys) are AES-256-GCM sealed in
  an envelope (fresh random 96-bit nonce per write, GCM tag).
- Master key resolution: `PROXYLLM_MASTER_KEY` ENV (32 bytes, hex/base64),
  else keyfile at `storage.master_key_path` (0600, generated on first
  start). The key never lives beside the data.
- The same master key doubles as the WebUI session/CSRF HMAC secret —
  sessions and at-rest secrets break and rotate together.
- Client keys and secrets never appear in config exports (placeholders
  only) and never in logs (see redaction).
- Key rotation is supported by the envelope wire format (version + key_id
  fields reserved); the rotation re-encryption task is a documented
  v1.1 item (see [configuration.md](configuration.md#master-key)).

## Logging redaction

Every log call site goes through the redaction layer
(`internal/observability`) — masks Bearer tokens, `sk-`/`fc-`/`pk-` keys,
JWTs, oversized base64, and AWS-style credentials, with an allowlist of
safe structured keys. Adversarial tests cover secrets in URLs, headers,
and JSON error bodies. The request log stores redacted error text and the
full `route_trace` (routing decisions, never credentials).

## Network and HTTP hardening

- **WebUI security headers**: CSP `default-src 'self'` (no inline script
  beyond nonce'd bootstrap), `X-Content-Type-Options: nosniff`,
  `Referrer-Policy`, `frame-ancestors 'none'`; HSTS when behind a TLS
  terminator.
- **API surface** forwards only documented header families (C3) and never
  echoes upstream credentials downstream.
- **Server timeouts**: `ReadHeaderTimeout` 10 s (Slowloris guard),
  `IdleTimeout` 120 s; deliberately NO `ReadTimeout`/`WriteTimeout` —
  they are absolute per-connection deadlines that would kill legitimate
  long SSE streams (streaming liveness is enforced by the executor's
  relayed-bytes watchdog, not the socket).
- **Body size limits** on all JSON endpoints; per-protocol validation
  before routing.

## Routing-side protections

- Model names are validated against the model table/aliases (no free-form
  upstream injection).
- Custom-provider base URLs: scheme/host validation with a private-address
  SSRF guard (documented opt-out for the operator).
- Rate limiting (token bucket) and concurrency caps per client key.
- Fallback/failover transitions are recorded in `route_trace` —
  fallbacks are never silent, so a degraded route is always visible.

## Container hardening

- Distroless static image, non-root UID 65532, read-only rootfs, dropped
  capabilities, no shell — see `docker/Dockerfile`,
  `docker/Dockerfile.release`, `docker/compose.yaml`.
- Compose adds `no-new-privileges`, `cap_drop: ALL`, `pids_limit`, tmpfs
  `/tmp`; the only writable path is the mounted `/data` (named volume
  or bind mount).
- In-binary healthcheck (`/proxyllm -healthcheck`) — distroless has no
  shell/curl.

## OAuth specifics (Codex provider)

- Callback binds to `127.0.0.1` only (ports 1455/1457).
- PKCE S256 mandatory; `state` validated.
- Tokens live only in the encrypted store; refresh runs server-side only.

## Reporting a vulnerability

See [SECURITY.md](../SECURITY.md) — please use private vulnerability
reporting, not public issues.