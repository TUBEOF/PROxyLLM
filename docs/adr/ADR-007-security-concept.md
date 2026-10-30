# ADR-007: Security — auth default-on, envelope encryption, redaction-first

- **Status**: Accepted
- **Date**: 2026-09-10

## Context

Threat model: LAN attacker or stolen client key. In scope:
unauthorized use, credential theft from disk/logs/responses, WebUI
escalation, quota burn, SSRF via custom-provider base URLs. Out of scope:
internet-scale multi-tenant exposure (documented hardening guidance only).

## Decision

1. **AuthN default-ON**: WebUI login (Argon2id) + client API keys (256-bit,
   display-once, keyed-MAC hash; legacy Argon2id rows upgrade on verify) on
   every client surface. Opt-out only via explicit ENV
   (`PROXYLLM_AUTH_DISABLED=1`, with a loud startup warning) — never
   default-off.
2. **Secret storage**: AES-256-GCM envelope per secret; master key from
   `PROXYLLM_MASTER_KEY` (ENV) or generated keyfile (0600); secrets never
   appear in config exports (placeholders). **Master-key rotation /
   re-encryption is not implemented** — changing the key requires a
   re-encryption migration that does not exist yet (documented limitation).
3. **Redaction as a tested layer, built FIRST**: all logging through
   `internal/observability/redact` — masks Bearer tokens, `sk-`/`fc-`/`pk_`
   keys, JWTs, oversized base64. Adversarial tests (secrets in URLs, headers,
   JSON error bodies). No log call site may bypass the redactor.
4. **Header policy**: strict WebUI security headers; the client surfaces
   forward only the allow-listed protocol headers (`anthropic-version`,
   `anthropic-beta`, `openai-beta`, `x-stainless-*`) to same-family
   upstreams; credentials and cookies are never forwarded.
5. **Input validation**: JSON body size limits, per-protocol schema
   validation before routing, custom-provider base-URL validation
   (scheme/host allowlist, private-address SSRF guard). **Per-client rate
   limiting and concurrency caps are not implemented** (documented in
   `docs/api/README.md` and `docs/security.md`).
6. **Container hardening**: distroless-style image, non-root UID, read-only
   rootfs (data bind mount is the only writable path), dropped capabilities,
   built-in healthcheck.
7. **OAuth specifics**: callback binds 127.0.0.1 only; PKCE S256 mandatory;
   state validated; tokens only in the encrypted store; refresh server-side
   only.

## Consequences

- Argon2id parameters follow OWASP Password Storage Cheat Sheet (verified
  live 2026-09-10): minimum m=19 MiB, t=2, p=1; hash time under 1s; PHC
  string format for storage.
- AES-256-GCM envelope: fresh random 12-byte nonce per encryption, stored
  alongside ciphertext; versioned envelope with key_id supports rotation
  (verified live 2026-09-10 against industry practice).
- The redaction layer existing before any other logging code is a
  structural guarantee, not a convention — new packages import it because
  nothing else existed first.