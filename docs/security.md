# Security

The threat model and the controls that answer it. The reasoning behind the
design is recorded in
[ADR-007](adr/ADR-007-security-concept.md) and
[ADR-016](adr/ADR-016-scalability-and-concurrency.md); this page is the
operational summary. To report a vulnerability, see
[SECURITY.md](../SECURITY.md).

## Scope

**In scope.** A single-tenant proxy operated by one operator, on a trusted
network or behind an authenticating reverse proxy. The threats addressed
here are unauthorized use of the model surfaces, credential theft from
disk, from logs and from responses, escalation through the console, quota
burn, and server-side request forgery through operator-supplied provider
URLs.

**Out of scope.** Internet-scale multi-tenant exposure of one deployment.
TLS termination, rate limiting and network hardening belong to the layer in
front — see [TLS](deployment.md#tls). Running the proxy directly on an
untrusted network without that layer is outside the design.

## Authentication

- **API keys** (`pk_<prefix>_<secret>`) gate both model surfaces. The secret
  is 256 bits of CSPRNG output, displayed exactly once at creation and
  regenerable or deletable in the console. Wire form and header handling are
  in the [API reference](api/README.md#authentication).
- **Console login** is per-admin. Passwords are hashed with Argon2id at the
  OWASP minimum parameter set (m=19 MiB, t=2, p=1) and compared in constant
  time; a login for an unknown username still pays one verification, so
  response time does not enumerate administrators.
- **Sessions** expire after 30 minutes idle or 12 hours absolute, whichever
  comes first, enforced server-side on every authenticated request. Only
  the SHA-256 of a session id is stored. The cookie is `HttpOnly`,
  `SameSite=Strict`, non-persistent, and `__Host-`-prefixed when the login
  arrived over TLS.
- **CSRF.** A mutating console request (POST, PUT, PATCH, DELETE) carries an
  `X-CSRF-Token` whose MAC is bound to the session cookie and compared in
  constant time. When `Origin` is present it must match the request's own
  scheme and host, and a `Content-Type` other than `application/json` is
  refused. The session and CSRF cookies are `SameSite=Strict` as well.
- **Default ON.** Authentication can be turned off only by the environment
  variable `PROXYLLM_AUTH_DISABLED=1` — never from the config file (an
  `auth` group there is a startup failure) and never from the console — and
  startup logs a loud warning when it is.
  See [configuration](configuration.md#environment-variables).
- **No admin API without a master key.** The console's admin routes are
  mounted only when a master key is wired. Without one, an unauthenticated
  request to `/api/admin/*` is answered by the client-key gate's uniform
  401, never a 404: whether the admin API is mounted is not revealed. Route
  and error shapes: [Admin API](api/admin.md).

## Secrets at rest

- Provider credentials and OAuth tokens are sealed with **AES-256-GCM** in
  an envelope: a fresh random 96-bit nonce per write, plus the GCM
  authentication tag. The nonce is generated per encryption — reuse would
  break the mode — never derived from the plaintext or the row.
- The **master key** resolves in this order: `PROXYLLM_MASTER_KEY` (32
  bytes, hex or base64), then the keyfile at `storage.master_key_path`,
  generated with mode 0600 on first start. It is held outside the
  configuration struct, so no configuration dump can carry it. See
  [Master key](configuration.md#master-key).
- The same master key is the **session and CSRF HMAC secret**. Sessions and
  at-rest secrets therefore break and rotate together: one key to lose, one
  key to protect.
- The envelope wire format reserves version and key-id fields, but the
  re-encryption task is not implemented — treat the master key as immutable
  and back it up with the database
  ([configuration.md](configuration.md#master-key)).

## API keys and the master key

An API key is 256 bits of CSPRNG output. It is stored as a **keyed MAC**,
not as a password hash:

```
$hmacsha256$v=1$<salt>$<mac>
```

The MAC is HMAC-SHA256 over the salt and the secret, keyed by
`HMAC-SHA256(masterKey, "proxyllm.client-key-hash/v1")` — a derivation that
runs once at startup, never per request. Verification compares in constant
time and fails closed on anything malformed, including a keyed-MAC row when
no MAC key is available. The per-row salt is 16 random bytes; the MAC key is
already secret, so the salt exists so that two rows holding the same secret
do not hash identically, not to make guessing harder.

Argon2id exists to make a low-entropy secret expensive to guess. There is
nothing to guess in 256 random bits, and the KDF was paid on *every*
authenticated request — a per-request cost that capped throughput long
before any security benefit appeared. **Administrator passwords keep
Argon2id**, for the opposite reason: they are human-chosen and
low-entropy.

The stored value carries its scheme, so rows written under the earlier
Argon2id scheme keep verifying and are rewritten on their first successful
use. No key has to be reissued, and a table holding both schemes is valid. A
stolen database alone is not enough to test guesses offline: the MAC key
lives in the process, not in the file.

A **delete** removes the row (name, prefix, hash): the secret stops
verifying immediately, and the key list shows only current keys. Historical
`request_logs.client_key_id` strings remain — there is no foreign key — and
the log's name projection falls back to the id. A **regenerate** keeps the
row id and name, replaces the prefix and hash, clears the last-used stamp
and returns the new secret exactly once; the old secret stops verifying at
the same moment.

**Consequence, stated plainly: rotating the master key invalidates every
API key.** They are display-once, so their secrets cannot be recovered to
be re-hashed, and every API key must be reissued after a rotation.
Rotation already requires re-entering provider credentials, so this widens
an operation the operator was going to perform anyway.

## Logging redaction

Every log call site passes through `internal/observability/redact`. The
redacting handler wraps every logger the application constructs, so a call
site cannot opt out — an unredacted log path would be a structural defect,
not an oversight. The inventory:

| Recognized | Kept visible |
|---|---|
| `Bearer <token>` | the scheme word |
| Credential headers (`x-api-key`, `x-auth-token`, `api-key`, `authorization`), with an optional scheme word such as `Token`, `Basic` or `Digest` | the header name |
| AWS-style access key IDs (`AKIA`/`ASIA` plus 16 characters) | — |
| Values in an explicit `secret (key)` context | the context words |
| `sk-`, `fc-` and `pk_` keys | — |
| `__Secure-session` values (the Ollama Cloud usage cookie) | the cookie name |
| JWTs (three base64url segments) | — |
| `key`, `token` and `secret` query parameters in URLs | the parameter name |
| JSON key/value secrets, including `usage_cookie`, `refresh_token` and `access_token` | the JSON key |
| Base64-ish blobs of 256 characters or more | — |

A masked value keeps its recognizable prefix, so an operator can tell what
kind of secret was present without seeing it. Benign content survives
because every pattern is anchored to a plausible secret shape — a model
name, a duration or an ordinary sentence is not a match. Redaction is
applied to values, not exempted per key: every string and byte-slice
attribute, and every message, passes through it.

The adversarial tests cover secrets in URLs, in headers and in JSON error
bodies. The request log stores the redacted error text and the `route_trace`
— routing decisions, never request bodies, headers or credentials — and a
captured payload passes the same redactor before it is written. That
matters most for a vision caption: it is model output derived from an
image, and an image is often a screenshot of a terminal, so a transcribed
credential is masked rather than stored. Images themselves are recorded by
media type and decoded size, never by their bytes.

## Network and HTTP hardening

- **Console headers.** The SPA is served with a
  `Content-Security-Policy` of `default-src 'self'; script-src 'self';
  object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors
  'none'`, plus `X-Content-Type-Options: nosniff` and
  `Referrer-Policy: same-origin`. The build emits no inline script, so no
  nonce is required. HSTS belongs at the TLS terminator
  ([deployment](deployment.md#tls)).
- **Only the credentials the provider owns.** An upstream request carries
  the provider's stored credential; a client's `Authorization` header is
  never relayed onward, and an upstream credential is never echoed
  downstream.
- **Timeouts.** `ReadHeaderTimeout` 10 s (Slowloris guard) and
  `IdleTimeout` 120 s. There is deliberately **no** `ReadTimeout` or
  `WriteTimeout`: both are absolute per-connection deadlines and would kill
  a legitimate long SSE stream mid-answer. Stream liveness is enforced one
  layer down instead, by the transport's idle watchdog on the upstream
  connection — five minutes with no bytes at all, measured on raw upstream
  bytes rather than on translated events, so a model that thinks for a long
  time between visible output is not mistaken for a stall.
- **Body limits.** 32 MiB on the client surfaces, 1 MiB on the admin API,
  and 4 KiB on the admin credential endpoints (bootstrap and password
  change). Requests are validated per protocol before routing.
- **SSRF guard.** A provider base URL supplied by the operator is checked
  for scheme and host, and an address resolving to loopback, RFC 1918,
  link-local (including the cloud metadata address) or unspecified space is
  refused — on the config write and again on every redirect the client
  follows. `PROXYLLM_ALLOW_PRIVATE_PROVIDER_URLS=1` is the documented
  opt-out, for deployments whose providers legitimately live on a private
  network; it stands both checks down, so it is a deliberate reduction of
  this control, not a tuning knob. See
  [configuration](configuration.md#environment-variables).

## OAuth (Codex provider)

- The console drives the **device flow**: the operator enters a `user_code`
  on the issuer's verification page. It needs no inbound endpoint at all,
  which is what makes it work through container isolation.
- The browser PKCE flow binds its callback to **loopback only** and refuses
  a non-loopback bind outright; the ports are 1455, with 1457 as the
  fallback. `code_challenge_method=S256` is mandatory, and `state` is
  compared both when the callback arrives and again before the code
  exchange.
- Tokens live only in the encrypted store. Refresh runs server-side only:
  single-flight, with single-use rotation of the refresh token and a lazy
  refresh before access-token expiry.

## Container hardening

The image, the compose profile and the healthcheck are described in
[deployment](deployment.md#container-hardening) — read-only rootfs, a
tmpfs `/tmp`, a memory limit paired with the in-binary probe, and the
reason the compose file runs as root.

## Reporting a vulnerability

See [SECURITY.md](../SECURITY.md) — use GitHub's private vulnerability
reporting, not a public issue.
