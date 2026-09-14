# Security Policy

## Supported versions

| Version | Supported |
|---|---|
| 1.0.x | ✅ |

## Reporting a vulnerability

Please report vulnerabilities via GitHub's **private vulnerability
reporting** (Security tab → "Report a vulnerability") — do **not** open a
public issue.

Include what you can of:

- affected component/endpoint and version
- reproduction steps (a minimal request is ideal)
- impact assessment

You will receive an acknowledgment within **7 days** and a status update
at least every 14 days until resolution. Credit in the release notes is
default (opt out by saying so).

## Scope

In scope: the PROxyLLM proxy binary and its WebUI, the client-facing API
surfaces, the admin API, the bundled Docker setup, and the release
artifacts themselves (binary/image integrity).

Out of scope for v1 (documented limitation): internet-scale multi-tenant
exposure of a single proxy deployment — see
[docs/security.md](docs/security.md) for the at-operator's-risk hardening
guidance. Deployment-specific misconfiguration (e.g. disabling auth via
ENV deliberately) is out of scope.

## Safe harbor

Good-faith research within this policy is welcomed; we will not pursue
action against you for responsibly reported issues.