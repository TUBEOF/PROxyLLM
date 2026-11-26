# ADR-022: Anthropic OAuth provider (Teams/Enterprise)

- **Status**: Accepted
- **Date**: 2026-11-26

## Context

The v2 provider set asks for Anthropic OAuth as a first-class account type.
Anthropic's consumer OAuth (Free/Pro/Max subscriptions) is not usable here:
since 2026-01-09 the servers reject those credentials outside Claude.ai and
the Claude Code CLI with "This credential is only authorized for use with
Claude Code and cannot be used for other API requests", and Anthropic's legal
and compliance documentation prohibits consumer-plan OAuth tokens in
third-party tools. OpenCode removed Claude Pro/Max authentication for the same
reason. Consumer support is therefore out of scope, and no client-identity or
user-agent cloaking is contemplated: it is an arms race, it is not
maintainable, and it sits in a terms-of-service grey zone.

Teams/Enterprise OAuth is the legitimate path. An organization can still
disable OAuth ("OAuth authentication is currently not allowed for this
organization"), so this is a best-effort path that depends on the workspace
policy — not a promise.

Facts that shape the design (Claude Code OAuth, current at the time of
writing):

- authorize: `https://claude.ai/oauth/authorize`, PKCE `S256`;
- client id: the Claude Code client id
  `9d1c250a-e61b-44d9-88ed-5944d1962f5e`;
- scopes: `org:create_api_key user:profile user:inference`;
- token endpoint: `https://platform.claude.com/v1/oauth/token`, JSON body
  (not form-encoded);
- access token: 8 hours (`expires_in: 28800`); refresh token long-lived and
  rotated;
- use: `Authorization: Bearer <access>` with
  `anthropic-beta: oauth-2025-04-20` and `anthropic-version` against
  `api.anthropic.com/v1/messages` — never `x-api-key`.

Anthropic has **no device flow**. The operator's browser is the only
interaction, and in a container or server deployment that browser is often not
on the same machine as the proxy.

## Criteria

- P0: no token, code or PKCE verifier leaves the server or reaches a log.
- P0: the flow works when the browser cannot reach the proxy (server/container).
- P0: login-CSRF is prevented — a callback is bound to the state the server
  issued.
- P1: the same shape as the existing Codex OAuth provider (auth, registry,
  token store, single-flight refresh, health states).
- P1: no consumer OAuth support, no cloaking, no undocumented endpoints.

## Options considered

| Option | Strengths | Weaknesses |
|---|---|---|
| Device flow | Best UX for servers | Anthropic does not offer one |
| Loopback callback only | Standard PKCE; no pasted secrets | Unreachable when the browser is on another machine |
| Paste-callback only | Works everywhere | Manual; the callback page shows "localhost not found" |
| Loopback + paste fallback (this ADR) | Works locally and in containers; one server-side flow state serves both | Two completion paths to test; the paste path needs state validation |

## Decision

New provider type `anthropic-oauth`, modelled on the Codex provider
(`internal/provider/codex`) and reusing the existing Anthropic Messages codec,
token store, envelope encryption, redaction and health tracking.

Flow: the admin UI starts a login; the server generates a 32-byte `state` and
a 43–128 character PKCE verifier, keeps both server-side for 15 minutes, and
allows one active flow per provider. The UI receives only the authorize URL
and a flow id.

Completion has two paths against the same server-side state:

1. **Paste (primary, documented default):** the operator signs in, lands on a
   "localhost not found" page (expected), and pastes either the full callback
   URL or `code#state` into the console. The server parses it, validates the
   state against the pending flow, and exchanges the code.
2. **Loopback (optional, same-machine installs):** the server listens on
   `127.0.0.1` only, on a configurable port (default 53692), accepts only
   `GET /callback`, validates the state, exchanges the code and closes.

The redirect URI is byte-identical in the authorize request and the exchange.
The verifier never leaves the server. Authorization codes, verifiers and
tokens are never logged; the redaction layer covers bearer values and the code
path is added explicitly.

Refresh is proactive (five-minute skew) with a single-flight guard, and
reactive on an upstream 401 only when no response byte has been written yet
(one forced refresh, one retry). A rotated refresh token is persisted together
with the access token in one transaction. `invalid_grant` marks the account
`auth_expired` — no automatic re-login (a human and a browser are required), no
token deletion, and the account leaves the rotation.

The console card carries a versioned risk notice and requires an explicit
acknowledgement, persisted server-side in the config KV under
`risk_ack:anthropic-oauth` with the notice version as value, so a text change
re-opens the warning for every admin and browser. The text states that only
Teams/Enterprise is supported, that consumer subscriptions are blocked by
Anthropic since January 2026 and violate its terms, and that use is at the
operator's own risk. The same text lives in the provider card and in the
provider documentation; the acknowledgement only suppresses repetition.

Not built: consumer support, UA/fingerprint cloaking, `sk-ant-oat01` static
tokens, the internal `create_api_key` endpoint, browser-side token storage,
automatic re-login.

## Consequences

- A new package, provider type, generalized admin OAuth routes (`start`,
  `complete`, `status` in addition to the Codex device poll), a risk-ack
  endpoint, a console modal and an M1 rotation cell.
- Wire facts become golden fixtures with source and date, and a
  `docs/research/anthropic-oauth.md` note records the citations.
- Five questions stay open until a Teams test account is available, and they
  are verified before the implementation lands: whether a third-party proxy
  may use the Claude Code client id (or an org-owned OAuth app exists), the
  minimal accepted scope set, whether arbitrary localhost ports are accepted,
  whether refresh rotation invalidates the previous token, and whether the
  beta header is required on every Anthropic endpoint. The fallbacks are
  documented in the work order; none of them changes the architecture.
- Consumer accounts produce a clear failure in the console and in the log,
  not a silent retry loop. The documentation says so before the operator tries.

## Sources

- Anthropic legal and compliance documentation (consumer-plan OAuth tokens
  are not usable in third-party tools), accessed 2026-11-26.
- Public reports of the 2026-01-09 server-side restriction
  (`developersdigest.tech/errors/opencode/claude-code-credential-only`,
  `midens.com/articles/2026-02-anthropic-oauth-ban-ai-tool-alternatives`),
  accessed 2026-11-26.
- Claude Code OAuth flow facts (authorize/token URLs, client id, PKCE,
  scopes, 8-hour access token) are transcribed from the v2.0.0 work order and
  are re-verified against a Teams test account before the implementation
  lands (open questions OQ-1..OQ-5).

