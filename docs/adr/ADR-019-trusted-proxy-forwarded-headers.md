# ADR-019: Trusted-proxy forwarded headers for the console's origin check

- **Status**: Accepted
- **Date**: 2026-10-23

## Context

The console's CSRF layer compares a mutating request's `Origin` header
against the origin the server reconstructs from the connection itself:
scheme from `r.TLS`, host from `r.Host`. A TLS terminator in front
(nginx, Caddy, Traefik — the setup `docs/deployment.md` recommends) breaks
both halves: it speaks plain HTTP to the server, so `r.TLS` is nil and
the scheme reconstructs as `http` while the browser sends
`Origin: https://…`, and the Host header may be the upstream address.
Every console mutation answered `403 origin mismatch` — the console was
unusable behind the very setup the deployment doc prescribes.

## Criteria

- P0: the console must work behind a TLS terminator.
- P0: a client that reaches the server directly must not spoof its way
  past the origin check by setting `X-Forwarded-*` itself.
- P1: zero-configuration deployments keep the exact current behavior.

## Options considered

| Option | Strengths | Weaknesses |
|---|---|---|
| Trust `X-Forwarded-*` unconditionally | No setup | Any direct client can spoof the origin; the CSRF origin layer becomes decorative |
| Public-base-URL config (reconstruct nothing, compare against a configured string) | Simple comparison | A second source of truth for the scheme/host; wrong for multi-host setups; still needs trust gating to be safe |
| Trusted-proxy CIDR set (nginx/Gin precedent) | Trust is explicit and bounded; no forwarded header is honored unless the peer proves it is configured | One more operator setting |

## Decision

`server.trusted_proxies` (config file) / `PROXYLLM_SERVER_TRUSTED_PROXIES`
(ENV) holds CIDRs or bare IPs. When the request's immediate peer is in
that set, the origin reconstruction honors `X-Forwarded-Proto` (only
`http`/`https`) and `X-Forwarded-Host` (leftmost element); otherwise it
uses the connection state exactly as before. Empty (the default) trusts
no one. The entries are validated at startup — a typo fails loudly, never
silently widens the boundary.

## Consequences

The console works behind a TLS terminator when the operator names the
proxy; the security posture of a default deployment is unchanged (an
untrusted client's forwarded headers are ignored —
TestCSRF_UntrustedForwardedHeadersIgnored pins that). The operator now
owns a trust decision: listing a range wider than the actual proxies
lets anything in that range spoof the public origin, so the docs
recommend the narrowest workable CIDR. Cookie semantics are unchanged:
the session cookie is still plain (no `__Host-` prefix) behind a
terminator, per the deployment doc's TLS section.