# Security Policy

## Supported versions

Security fixes target the latest released version.

## Reporting a vulnerability

**Do not open a public issue for security vulnerabilities.**

Please use GitHub's private vulnerability reporting
(GitHub → Security → Report a vulnerability). Include reproduction steps
and impact assessment. You will receive an acknowledgement within 72 hours.

## Security model

- **Authentication default-ON**: WebUI login + per-client API keys. Opt-out
  is only via explicit ENV (`PROXYLLM_AUTH_DISABLED=1`) with a loud startup
  warning — never default-off.
- **Secrets at rest**: provider credentials are AES-256-GCM-encrypted with
  a master key from ENV or a generated keyfile (0600). Client keys are
  stored as Argon2id hashes only; the raw secret is displayed exactly once
  at creation.
- **Redaction-first logging**: all logs flow through a tested redaction
  layer (Bearer tokens, `sk-`/`fc-`/`pk-` keys, JWTs, oversized base64).
- **Container hardening**: distroless image, non-root, read-only rootfs,
  dropped capabilities.

See [docs/adr/ADR-007-security-concept.md](docs/adr/ADR-007-security-concept.md)
for the full concept.

## Hardening for exposure beyond LAN

PROxyLLM v1 targets LAN/home-lab deployment behind a trusted network. If
you expose it further, put TLS termination in front (Caddy, nginx, Traefik)
and keep auth enabled. Internet-scale multi-tenant exposure is out of scope
for v1.