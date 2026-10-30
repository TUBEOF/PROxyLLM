# Security Policy

## Supported versions

| Version | Supported |
|---|---|
| 1.23.x | ✅ |
| older | ❌ |

Security fixes land on the current release line. Upgrading is a tag change —
the data directory and the master key carry over unchanged.

## Reporting a vulnerability

Please report vulnerabilities through GitHub's **private vulnerability
reporting** (repository → *Security* → *Report a vulnerability*). Do not open
a public issue for a suspected vulnerability.

Include what you can of:

- the affected component or endpoint, and the version
- reproduction steps — a minimal request is ideal
- your impact assessment

You will get an acknowledgment within **7 days**, and a status update at
least every 14 days until the report is resolved. Credit in the release
notes is the default; say so if you would rather stay anonymous.

## Scope

**In scope:** the proxy binary and its embedded console, the client-facing
API surfaces, the decision surfaces, the admin API, the bundled Docker
setup, and the release artifacts themselves.

**Out of scope:** internet-scale multi-tenant exposure of a single
deployment — the documented posture is one operator, one tenant, a trusted
network or an authenticating reverse proxy (see
[the security guide](docs/security.md)). Also out of scope: consequences of
deliberately weakening the configuration, such as disabling authentication
through the documented environment variable.

## Safe harbor

Good-faith research within this policy is welcome. We will not pursue action
against you for issues you report responsibly.
